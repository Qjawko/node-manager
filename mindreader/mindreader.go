// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mindreader

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/bstream/blockstream"
	"github.com/dfuse-io/dstore"
	nodeManager "github.com/dfuse-io/node-manager"
	"github.com/dfuse-io/shutter"
	"go.uber.org/zap"
)

type ConsolerReader interface {
	Read() (obj interface{}, err error)
	Done() <-chan interface{}
}

type ConsolerReaderFactory func(reader io.Reader) (ConsolerReader, error)

// ConsoleReaderBlockTransformer is a function that accepts an `obj` of type
// `interface{}` as produced by a specialized ConsoleReader implementation and
// turns it into a `bstream.Block` that is able to flow in block streams.
type ConsoleReaderBlockTransformer func(obj interface{}) (*bstream.Block, error)

type MindReaderPlugin struct {
	*shutter.Shutter

	writer              *io.PipeWriter
	consoleReader       ConsolerReader
	consumeReadFlowDone chan interface{}
	continuityChecker   ContinuityChecker
	transformer         ConsoleReaderBlockTransformer
	archiver            Archiver
	gator               Gator
	stopAtBlockNum      uint64
	channelCapacity     int
	blockServer         *blockstream.Server

	headBlockUpdateFunc nodeManager.HeadBlockUpdater

	setMaintenanceFunc func()
	stopBlockReachFunc func()
	zlogger            *zap.Logger
}

func NewMindReaderPlugin(
	archiveStoreURL string,
	mergeArchiveStoreURL string,
	mergeUploadDirectly bool,
	workingDirectory string,
	consoleReaderFactory ConsolerReaderFactory,
	consoleReaderTransformer ConsoleReaderBlockTransformer,
	startBlockNum uint64,
	stopBlockNum uint64,
	discardAfterStopBlock bool,
	channelCapacity int,
	headBlockUpdateFunc nodeManager.HeadBlockUpdater,
	setMaintenanceFunc func(),
	stopBlockReachFunc func(),
	failOnNonContinuousBlocks bool,
	zlogger *zap.Logger,
) (*MindReaderPlugin, error) {
	archiveStore, err := dstore.NewDBinStore(archiveStoreURL)
	if err != nil {
		return nil, fmt.Errorf("setting up archive store: %w", err)
	}
	archiveStore.SetOverwrite(true)

	gator := NewBlockNumberGator(startBlockNum)

	// Create directory and its parent(s), it's a no-op if everything already exists
	err = os.MkdirAll(workingDirectory, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("unable to create working directory %q: %w", workingDirectory, err)
	}

	var continuityChecker ContinuityChecker
	cc, err := NewContinuityChecker(filepath.Join(workingDirectory, "continuity_check"), zlogger)
	if err != nil {
		return nil, fmt.Errorf("error setting up continuity checker: %s", err)
	}

	if failOnNonContinuousBlocks {
		continuityChecker = cc
	} else {
		cc.Reset()
	}

	var archiver Archiver
	if mergeUploadDirectly {
		zlogger.Debug("using merge and upload directly mode")
		mergeArchiveStore, err := dstore.NewDBinStore(mergeArchiveStoreURL)
		if err != nil {
			return nil, fmt.Errorf("setting up merge archive store: %w", err)
		}
		mergeArchiveStore.SetOverwrite(true)

		var options []MergeArchiverOption
		if stopBlockNum != 0 {
			if discardAfterStopBlock {
				zlogger.Info("archive store will discard any block after stop block, this will create a hole in block files after restart", zap.Uint64("stop_block_num", stopBlockNum))
			} else {
				zlogger.Info("blocks after stop-block-num will be saved to Oneblock files to be merged afterwards", zap.Uint64("stop_block_num", stopBlockNum))
				oneblockArchiver := NewOneblockArchiver(workingDirectory, archiveStore, bstream.GetBlockWriterFactory, 0, zlogger)
				options = append(options, WithOverflowArchiver(oneblockArchiver))
			}
		}
		ra := NewMergeArchiver(mergeArchiveStore, bstream.GetBlockWriterFactory, stopBlockNum, zlogger, options...)
		archiver = ra
	} else {
		var archiverStopBlockNum uint64
		if stopBlockNum != 0 && discardAfterStopBlock {
			zlogger.Info("archive store will discard any block after stop block, this will create a hole in block files after restart", zap.Uint64("stop_block_num", stopBlockNum))
			archiverStopBlockNum = stopBlockNum
		}
		archiver = NewOneblockArchiver(workingDirectory, archiveStore, bstream.GetBlockWriterFactory, archiverStopBlockNum, zlogger)
	}

	if err := archiver.Init(); err != nil {
		return nil, fmt.Errorf("failed to init archiver: %w", err)
	}

	mindReaderPlugin, err := newMindReaderPlugin(archiver, consoleReaderFactory, consoleReaderTransformer, continuityChecker, gator, stopBlockNum, channelCapacity, headBlockUpdateFunc, zlogger)
	if err != nil {
		return nil, err
	}
	mindReaderPlugin.setMaintenanceFunc = setMaintenanceFunc
	mindReaderPlugin.stopBlockReachFunc = stopBlockReachFunc

	mindReaderPlugin.OnTerminating(func(_ error) {
		zlogger.Info("mindreader plugin OnTerminating called")
		mindReaderPlugin.setMaintenanceFunc()
		mindReaderPlugin.waitForReadFlowToComplete()
		if stopBlockNum != 0 {
			mindReaderPlugin.stopBlockReachFunc()
		}
	})

	return mindReaderPlugin, nil
}

func (p *MindReaderPlugin) Run(server *blockstream.Server) {
	p.zlogger.Info("running")
	p.blockServer = server
	go p.ReadFlow()
}

func newMindReaderPlugin(
	archiver Archiver,
	consoleReaderFactory ConsolerReaderFactory,
	consoleReaderTransformer ConsoleReaderBlockTransformer,
	continuityChecker ContinuityChecker,
	gator Gator,
	stopAtBlockNum uint64,
	channelCapacity int,
	headBlockUpdateFunc nodeManager.HeadBlockUpdater,
	zlogger *zap.Logger,
) (*MindReaderPlugin, error) {
	pipeReader, pipeWriter := io.Pipe()
	consoleReader, err := consoleReaderFactory(pipeReader)
	if err != nil {
		return nil, err
	}
	zlogger.Info("creating new mindreader plugin")
	return &MindReaderPlugin{
		Shutter:             shutter.New(),
		consoleReader:       consoleReader,
		continuityChecker:   continuityChecker,
		consumeReadFlowDone: make(chan interface{}),
		transformer:         consoleReaderTransformer,
		writer:              pipeWriter,
		archiver:            archiver,
		gator:               gator,
		stopAtBlockNum:      stopAtBlockNum,
		channelCapacity:     channelCapacity,
		headBlockUpdateFunc: headBlockUpdateFunc,
		zlogger:             zlogger,
	}, nil
}

func (p *MindReaderPlugin) waitForReadFlowToComplete() {
	p.zlogger.Info("waiting until consume read flow (i.e. blocks) is actually done processing blocks...")
	<-p.consumeReadFlowDone
}

func (p *MindReaderPlugin) ReadFlow() {
	p.zlogger.Info("starting read flow")
	blocks := make(chan *bstream.Block, p.channelCapacity)

	go p.consumeReadFlow(blocks)
	go p.alwaysUploadFiles()

	for {
		// ALWAYS READ (otherwise you'll stall `nodeos`' shutdown process, want a dirty flag?)
		err := p.readOneMessage(blocks)
		if err != nil {
			if err == io.EOF {
				p.zlogger.Info("reached end of console reader stream, nothing more to do")
				return
			}
			p.zlogger.Error("reading from console logs", zap.Error(err))
			p.setMaintenanceFunc()
			continue
		}
	}
}

func (p *MindReaderPlugin) alwaysUploadFiles() {
	p.zlogger.Info("starting file upload")
	for {
		if p.IsTerminating() { // the uploadFiles will be called again in 'WaitForAllFilesToUpload()', we can leave here early
			return
		}

		if err := p.archiver.uploadFiles(); err != nil {
			p.zlogger.Warn("failed to upload stale files", zap.Error(err))
		}

		select {
		case <-p.Terminating():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// consumeReadFlow is the one function blocking termination until consumption/writeBlock/upload is done
func (p *MindReaderPlugin) consumeReadFlow(blocks <-chan *bstream.Block) {
	p.zlogger.Info("starting consume flow")

	defer func() {
		p.archiver.WaitForAllFilesToUpload()
		p.zlogger.Debug("archiver WaitForAllFilesToUpload done")
		close(p.consumeReadFlowDone)
	}()

	for {
		select {
		case <-p.Terminating():
			if len(blocks) == 0 {
				p.zlogger.Debug("all blocks in channel were drained, exiting read flow")
				return
			}

			p.zlogger.Info("waiting for blocks channel to drain, going to quit when no more blocks in channel", zap.Int("block_count", len(blocks)))

		case block := <-blocks:
			err := p.archiver.storeBlock(block)
			if err != nil {
				p.zlogger.Error("failed storing block in archiver", zap.Error(err))
				p.Shutdown(fmt.Errorf("archiver store block failed: %w", err))
				return
			}

			if p.continuityChecker != nil {
				err = p.continuityChecker.Write(block.Num())
				if err != nil {
					p.zlogger.Error("failed continuity check", zap.Error(err))
					p.setMaintenanceFunc()
					continue
				}
			}

			if p.blockServer != nil {
				err = p.blockServer.PushBlock(block)
				if err != nil {
					p.zlogger.Error("failed passing block to blockServer", zap.Error(err))
					p.Shutdown(fmt.Errorf("failed writing to blocks server handler: %w", err))
					return
				}

			}
		}
	}
}

func (p *MindReaderPlugin) readOneMessage(blocks chan<- *bstream.Block) error {
	obj, err := p.consoleReader.Read()
	if err != nil {
		return err
	}

	block, err := p.transformer(obj)
	if err != nil {
		return fmt.Errorf("unable to transform console read obj to bstream.Block: %w", err)
	}

	if !p.gator.pass(block) {
		return nil
	}

	if p.headBlockUpdateFunc != nil {
		p.headBlockUpdateFunc(block.Num(), block.ID(), block.Time())
	}

	blocks <- block

	if p.stopAtBlockNum != 0 && block.Num() >= p.stopAtBlockNum && !p.IsTerminating() {
		p.zlogger.Info("shutting down because requested end block reached", zap.Uint64("block_num", block.Num()))
		go p.Shutdown(nil)
	}

	return nil
}

func (p *MindReaderPlugin) Close(err error) {
	p.zlogger.Info("closing pipe writer and shutting down plugin")
	p.writer.CloseWithError(err)
	p.Shutdown(err)
}

// LogLine receives log line and write it to "pipe" of the local console reader
func (p *MindReaderPlugin) LogLine(in string) {
	if _, err := p.writer.Write(append([]byte(in), '\n')); err != nil {
		p.zlogger.Error("writing to export pipeline", zap.Error(err))
		p.Shutdown(err)
	}
}

func (p *MindReaderPlugin) HasContinuityChecker() bool {
	return p.continuityChecker != nil
}

func (p *MindReaderPlugin) ResetContinuityChecker() {
	if p.continuityChecker != nil {
		p.continuityChecker.Reset()
	}
}

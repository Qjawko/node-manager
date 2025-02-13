package mindreader

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/shutter"
)

func TestMindReaderPlugin_ReadFlow(t *testing.T) {
	numOfLines := 1
	lines := make(chan string, numOfLines)
	blocks := make(chan *bstream.Block, numOfLines)

	mindReader := &MindReaderPlugin{
		Shutter:       shutter.New(),
		lines:         lines,
		consoleReader: newTestConsoleReader(lines),
		startGate:     NewBlockNumberGate(1),
	}

	wg := sync.WaitGroup{}
	wg.Add(numOfLines)

	var readMessageError error
	go func() {
		defer wg.Done()
		readMessageError = mindReader.readOneMessage(blocks)
	}()

	mindReader.LogLine(`DMLOG {"id":"00000001a"}`)
	select {
	case b := <-blocks:
		require.Equal(t, uint64(01), b.Number)
	case <-time.After(time.Second):
		t.Error("too long")
	}

	wg.Wait()
	require.NoError(t, readMessageError)
}

func TestMindReaderPlugin_GatePassed(t *testing.T) {
	numOfLines := 2
	lines := make(chan string, numOfLines)
	blocks := make(chan *bstream.Block, numOfLines)

	mindReader := &MindReaderPlugin{
		Shutter:       shutter.New(),
		lines:         lines,
		consoleReader: newTestConsoleReader(lines),
		startGate:     NewBlockNumberGate(2),
	}

	mindReader.LogLine(`DMLOG {"id":"00000001a"}`)
	mindReader.LogLine(`DMLOG {"id":"00000002a"}`)

	wg := sync.WaitGroup{}
	wg.Add(numOfLines)

	readErrors := []error{}
	go func() {
		for i := 0; i < numOfLines; i++ {
			err := mindReader.readOneMessage(blocks)
			readErrors = append(readErrors, err)
			wg.Done()
		}
	}()

	select {
	case b := <-blocks:
		require.Equal(t, uint64(02), b.Number)
	case <-time.After(time.Second):
		t.Error("too long")
	}

	wg.Wait()
	for _, err := range readErrors {
		require.NoError(t, err)
	}
}

func TestMindReaderPlugin_StopAtBlockNumReached(t *testing.T) {
	numOfLines := 2
	lines := make(chan string, numOfLines)
	blocks := make(chan *bstream.Block, numOfLines)
	done := make(chan interface{})

	mindReader := &MindReaderPlugin{
		Shutter:       shutter.New(),
		lines:         lines,
		consoleReader: newTestConsoleReader(lines),
		startGate:     NewBlockNumberGate(0),
		stopBlock:     2,
		zlogger:       testLogger,
	}
	mindReader.OnTerminating(func(err error) {
		if err == nil {
			close(done)
		} else {
			t.Error("should not be called")
		}
	})

	mindReader.LogLine(`DMLOG {"id":"00000001a"}`)
	mindReader.LogLine(`DMLOG {"id":"00000002a"}`)

	wg := sync.WaitGroup{}
	wg.Add(numOfLines)

	readErrors := []error{}
	go func() {
		for i := 0; i < numOfLines; i++ {
			err := mindReader.readOneMessage(blocks)
			readErrors = append(readErrors, err)
			wg.Done()
		}
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Millisecond):
		t.Error("too long")
	}

	wg.Wait()
	for _, err := range readErrors {
		require.NoError(t, err)
	}

	// Validate actually read block
	assert.Equal(t, numOfLines, len(blocks)) // moderate requirement, race condition can make it pass more blocks
}

func TestMindReaderPlugin_OneBlockSuffixFormat(t *testing.T) {
	assert.Error(t, validateOneBlockSuffix(""))
	assert.NoError(t, validateOneBlockSuffix("example"))
	assert.NoError(t, validateOneBlockSuffix("example-hostname-123"))
	assert.NoError(t, validateOneBlockSuffix("example_hostname_123"))
	assert.Equal(t, `oneblock_suffix contains invalid characters: "example.lan"`, validateOneBlockSuffix("example.lan").Error())
}

type testConsoleReader struct {
	lines chan string
	done  chan interface{}
}

func newTestConsoleReader(lines chan string) *testConsoleReader {
	return &testConsoleReader{
		lines: lines,
	}
}

func (c *testConsoleReader) Done() <-chan interface{} {
	return c.done
}

func (c *testConsoleReader) ReadBlock() (*bstream.Block, error) {
	line, _ := <-c.lines
	formatedLine := line[6:]

	type block struct {
		ID string `json:"id"`
	}

	data := new(block)
	if err := json.Unmarshal([]byte(formatedLine), data); err != nil {
		return nil, fmt.Errorf("marshalling error on '%s': %w", formatedLine, err)
	}
	return &bstream.Block{
		Id:     data.ID,
		Number: toBlockNum(data.ID),
	}, nil
}

func toBlockNum(blockID string) uint64 {
	if len(blockID) < 8 {
		return 0
	}
	bin, err := hex.DecodeString(blockID[:8])
	if err != nil {
		return 0
	}
	return uint64(binary.BigEndian.Uint32(bin))
}

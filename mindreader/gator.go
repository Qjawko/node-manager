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
	"github.com/streamingfast/bstream"
)

type BlockNumberGate struct {
	passed   bool
	blockNum uint64
}

func NewBlockNumberGate(blockNum uint64) *BlockNumberGate {
	return &BlockNumberGate{
		blockNum: blockNum,
	}
}

func (g *BlockNumberGate) pass(block *bstream.Block) bool {
	if g.passed {
		return true
	}

	g.passed = block.Num() >= g.blockNum
	return g.passed
}

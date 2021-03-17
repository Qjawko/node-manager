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

package logplugin

import "github.com/dfuse-io/bstream/blockstream"

type LogPlugin interface {
	Launch()
	LogLine(in string)
	Close(err error)
}

type Shutter interface {
	Terminated() <-chan struct{}
	OnTerminating(f func(error))
	OnTerminated(f func(error))
	Shutdown(err error)
}

type BlockStreamer interface {
	Run(blockServer *blockstream.Server)
}

type LogPluginFunc func(line string)

func (f LogPluginFunc) Launch()             {}
func (f LogPluginFunc) LogLine(line string) { f(line) }

func (f LogPluginFunc) Close(_ error) {}

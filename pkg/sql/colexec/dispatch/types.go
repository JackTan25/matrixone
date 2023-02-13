// Copyright 2021 Matrix Origin
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

package dispatch

import (
	"context"

	"github.com/google/uuid"
	"github.com/matrixorigin/matrixone/pkg/cnservice/cnclient"
	"github.com/matrixorigin/matrixone/pkg/common/morpc"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/pb/pipeline"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

type WrapperClientSession struct {
	msgId uint64
	ctx   context.Context
	cs    morpc.ClientSession
	uuid  uuid.UUID
	// toAddr string
}
type container struct {
	// the clientsession info for the channel you want to dispatch
	remoteReceivers []*WrapperClientSession
	// sendFunc is the rule you want to send batch
	sendFunc func(bat *batch.Batch, ap *Argument, proc *process.Process) error
}

type Argument struct {
	ctr      *container
	prepared bool
	sendto   int

	// FuncId means the sendFunc
	FuncId int
	// LocalRegs means the local register you need to send to.
	LocalRegs []*process.WaitRegister
	// RemoteRegs specific the remote reg you need to send to.
	RemoteRegs []colexec.ReceiveInfo
}

func (arg *Argument) Free(proc *process.Process, pipelineFailed bool) {
	if arg.ctr.remoteReceivers != nil {
		// TODO: how to handle pipelineFailed?
		for _, r := range arg.ctr.remoteReceivers {
			message := cnclient.AcquireMessage()
			{
				message.Id = r.msgId
				message.Cmd = pipeline.BatchMessage
				message.Sid = pipeline.MessageEnd
				message.Uuid = r.uuid[:]
			}
			r.cs.Write(r.ctx, message)
		}

	}

	if pipelineFailed {
		for i := range arg.LocalRegs {
			for len(arg.LocalRegs[i].Ch) > 0 {
				bat := <-arg.LocalRegs[i].Ch
				if bat == nil {
					break
				}
				bat.Clean(proc.Mp())
			}
		}
	}

	for i := range arg.LocalRegs {
		select {
		case <-arg.LocalRegs[i].Ctx.Done():
		case arg.LocalRegs[i].Ch <- nil:
		}
		close(arg.LocalRegs[i].Ch)
	}
}

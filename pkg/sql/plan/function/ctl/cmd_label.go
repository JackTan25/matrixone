// Copyright 2021 - 2023 Matrix Origin
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

package ctl

import (
	"context"
	"fmt"
	"github.com/matrixorigin/matrixone/pkg/clusterservice"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/ctlservice"
	pb "github.com/matrixorigin/matrixone/pkg/pb/ctl"
	"github.com/matrixorigin/matrixone/pkg/pb/metadata"
	"github.com/matrixorigin/matrixone/pkg/pb/timestamp"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
	"regexp"
	"strings"
	"time"
)

type cnLabel struct {
	uuid   string
	key    string
	values []string
}

type parser interface {
	parse() cnLabel
}

type singleValue struct {
	s   string
	reg *regexp.Regexp
}

func newSingleValue(s string, reg *regexp.Regexp) *singleValue {
	return &singleValue{
		s:   s,
		reg: reg,
	}
}

func (s *singleValue) parse() cnLabel {
	items := s.reg.FindStringSubmatch(s.s)
	var c cnLabel
	c.uuid = items[1]
	c.key = items[2]
	c.values = []string{items[3]}
	return c
}

type multiValues struct {
	s   string
	reg *regexp.Regexp
}

func newMultiValue(s string, reg *regexp.Regexp) *multiValues {
	return &multiValues{
		s:   s,
		reg: reg,
	}
}

func (m *multiValues) parse() cnLabel {
	items := m.reg.FindStringSubmatch(m.s)
	var c cnLabel
	c.uuid = items[1]
	c.key = items[2]
	c.values = strings.Split(items[3], ",")
	return c
}

func identifyParser(param string) parser {
	singlePattern := `^([a-zA-Z0-9\-_]+):([a-zA-Z0-9_]+):([a-zA-Z0-9_]+)$`
	multiplePattern := `^([a-zA-Z0-9\-_]+):([a-zA-Z0-9_]+):\[([a-zA-Z0-9_]+(,[a-zA-Z0-9_]+)*)\]$`
	matched, err := regexp.MatchString(singlePattern, param)
	if err != nil {
		return nil
	} else if matched {
		return newSingleValue(param, regexp.MustCompile(singlePattern))
	}
	matched, err = regexp.MatchString(multiplePattern, param)
	if err != nil {
		return nil
	} else if matched {
		return newMultiValue(param, regexp.MustCompile(multiplePattern))
	}
	return nil
}

// parseParameter parses the parameter which contains CN uuid and its label
// information. Its format can be: (1) cn:key:value (2) cn:key:[v1,v2,v3]
func parseCNLabel(param string) (cnLabel, error) {
	p := identifyParser(param)
	if p == nil {
		return cnLabel{}, moerr.NewInternalErrorNoCtx("format is: cn:key:value or cn:key:[v1,v2,...]")
	}
	return p.parse(), nil
}

func handleSetLabel(proc *process.Process,
	service serviceType,
	parameter string,
	sender requestSender) (pb.CtlResult, error) {
	cluster := clusterservice.GetMOCluster()
	c, err := parseCNLabel(parameter)
	if err != nil {
		return pb.CtlResult{}, err
	}
	kvs := make(map[string][]string)
	kvs[c.key] = c.values
	if err := cluster.DebugUpdateCNLabel(c.uuid, kvs); err != nil {
		return pb.CtlResult{}, err
	}
	return pb.CtlResult{
		Method: pb.CmdMethod_Label.String(),
		Data:   "OK",
	}, nil
}

func handleSyncCommit(
	proc *process.Process,
	service serviceType,
	parameter string,
	sender requestSender) (pb.CtlResult, error) {
	cs := ctlservice.GetCtlService()
	mc := clusterservice.GetMOCluster()
	var services []string
	mc.GetCNService(
		clusterservice.NewSelector(),
		func(c metadata.CNService) bool {
			services = append(services, c.ServiceID)
			return true
		})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	maxCommitTS := timestamp.Timestamp{}
	for _, id := range services {
		resp, err := cs.SendCtlMessage(
			ctx,
			metadata.ServiceType_CN,
			id,
			cs.NewRequest(pb.CmdMethod_GetCommit))
		if err != nil {
			return pb.CtlResult{}, err
		}
		if maxCommitTS.Less(resp.GetCommit.CurrentCommitTS) {
			maxCommitTS = resp.GetCommit.CurrentCommitTS
		}
		cs.Release(resp)
	}

	for _, id := range services {
		req := cs.NewRequest(pb.CmdMethod_SyncCommit)
		req.SycnCommit.LatestCommitTS = maxCommitTS
		resp, err := cs.SendCtlMessage(
			ctx,
			metadata.ServiceType_CN,
			id,
			req)
		if err != nil {
			return pb.CtlResult{}, err
		}
		cs.Release(resp)
	}

	return pb.CtlResult{
		Method: pb.CmdMethod_SyncCommit.String(),
		Data: fmt.Sprintf("sync %d cn services's commit ts to %s",
			len(services),
			maxCommitTS.DebugString()),
	}, nil
}

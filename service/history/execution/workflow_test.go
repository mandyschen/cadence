// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package execution

import (
	"context"
	"reflect"
	"runtime"
	"testing"

	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/uber/cadence/common/activecluster"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/log/testlogger"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/types"
)

type (
	workflowSuite struct {
		suite.Suite
		*require.Assertions

		controller       *gomock.Controller
		mockContext      *MockContext
		mockMutableState *MockMutableState

		domainID   string
		domainName string
		workflowID string
		runID      string
	}
)

func TestWorkflowSuite(t *testing.T) {
	s := new(workflowSuite)
	suite.Run(t, s)
}

func (s *workflowSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockContext = NewMockContext(s.controller)
	s.mockMutableState = NewMockMutableState(s.controller)
	s.domainID = uuid.New()
	s.domainName = "domain-name"
	s.workflowID = "some random workflow ID"
	s.runID = uuid.New()
}

func (s *workflowSuite) TearDownTest() {
	s.controller.Finish()
}

func (s *workflowSuite) TestGetMethods() {
	lastEventTaskID := int64(144)
	lastEventVersion := int64(12)
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastEventVersion, nil).AnyTimes()
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           s.runID,
		LastEventTaskID: lastEventTaskID,
	}).AnyTimes()

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)

	s.Equal(s.mockContext, nDCWorkflow.GetContext())
	s.Equal(s.mockMutableState, nDCWorkflow.GetMutableState())
	// NOTE golang does not seem to let people compare functions, easily
	//  link: https://github.com/stretchr/testify/issues/182
	// this is a hack to compare 2 functions, being the same
	expectedReleaseFn := runtime.FuncForPC(reflect.ValueOf(NoopReleaseFn).Pointer()).Name()
	actualReleaseFn := runtime.FuncForPC(reflect.ValueOf(nDCWorkflow.GetReleaseFn()).Pointer()).Name()
	s.Equal(expectedReleaseFn, actualReleaseFn)
	version, taskID, err := nDCWorkflow.GetVectorClock()
	s.NoError(err)
	s.Equal(lastEventVersion, version)
	s.Equal(lastEventTaskID, taskID)
}

func (s *workflowSuite) TestHappensAfter_LargerVersion() {
	thisLastWriteVersion := int64(0)
	thisLastEventTaskID := int64(100)
	thatLastWriteVersion := thisLastWriteVersion - 1
	thatLastEventTaskID := int64(123)

	s.True(workflowHappensAfter(
		thisLastWriteVersion,
		thisLastEventTaskID,
		thatLastWriteVersion,
		thatLastEventTaskID,
	))
}

func (s *workflowSuite) TestHappensAfter_SmallerVersion() {
	thisLastWriteVersion := int64(0)
	thisLastEventTaskID := int64(100)
	thatLastWriteVersion := thisLastWriteVersion + 1
	thatLastEventTaskID := int64(23)

	s.False(workflowHappensAfter(
		thisLastWriteVersion,
		thisLastEventTaskID,
		thatLastWriteVersion,
		thatLastEventTaskID,
	))
}

func (s *workflowSuite) TestHappensAfter_SameVersion_SmallerTaskID() {
	thisLastWriteVersion := int64(0)
	thisLastEventTaskID := int64(100)
	thatLastWriteVersion := thisLastWriteVersion
	thatLastEventTaskID := thisLastEventTaskID + 1

	s.False(workflowHappensAfter(
		thisLastWriteVersion,
		thisLastEventTaskID,
		thatLastWriteVersion,
		thatLastEventTaskID,
	))
}

func (s *workflowSuite) TestHappensAfter_SameVersion_LargerTaskID() {
	thisLastWriteVersion := int64(0)
	thisLastEventTaskID := int64(100)
	thatLastWriteVersion := thisLastWriteVersion
	thatLastEventTaskID := thisLastEventTaskID - 1

	s.True(workflowHappensAfter(
		thisLastWriteVersion,
		thisLastEventTaskID,
		thatLastWriteVersion,
		thatLastEventTaskID,
	))
}

func (s *workflowSuite) TestSuppressWorkflowBy_Error() {
	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)

	incomingMockContext := NewMockContext(s.controller)
	incomingMockMutableState := NewMockMutableState(s.controller)
	incomingNDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		incomingMockContext,
		incomingMockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)

	// cannot suppress by older workflow
	lastEventTaskID := int64(144)
	lastEventVersion := int64(12)
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastEventVersion, nil).AnyTimes()
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           s.runID,
		LastEventTaskID: lastEventTaskID,
	}).AnyTimes()

	incomingRunID := uuid.New()
	incomingLastEventTaskID := int64(144)
	incomingLastEventVersion := lastEventVersion - 1
	incomingMockMutableState.EXPECT().GetLastWriteVersion().Return(incomingLastEventVersion, nil).AnyTimes()
	incomingMockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           incomingRunID,
		LastEventTaskID: incomingLastEventTaskID,
	}).AnyTimes()

	_, err := nDCWorkflow.SuppressBy(incomingNDCWorkflow)
	s.Error(err)
}

func (s *workflowSuite) TestSuppressWorkflowBy_Terminate() {
	lastEventID := int64(2)
	lastEventTaskID := int64(144)
	lastEventVersion := cluster.TestCurrentClusterInitialFailoverVersion
	s.mockMutableState.EXPECT().GetNextEventID().Return(lastEventID + 1).AnyTimes()
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastEventVersion, nil).AnyTimes()
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           s.runID,
		LastEventTaskID: lastEventTaskID,
	}).AnyTimes()
	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)

	incomingRunID := uuid.New()
	incomingLastEventTaskID := int64(144)
	incomingLastEventVersion := lastEventVersion + 1
	incomingMockContext := NewMockContext(s.controller)
	incomingMockMutableState := NewMockMutableState(s.controller)
	incomingNDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		incomingMockContext,
		incomingMockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	incomingMockMutableState.EXPECT().GetLastWriteVersion().Return(incomingLastEventVersion, nil).AnyTimes()
	incomingMockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           incomingRunID,
		LastEventTaskID: incomingLastEventTaskID,
	}).AnyTimes()

	s.mockMutableState.EXPECT().UpdateCurrentVersion(lastEventVersion, true).Return(nil).AnyTimes()
	inFlightDecision := &DecisionInfo{
		Version:    1234,
		ScheduleID: 5678,
		StartedID:  9012,
	}
	s.mockMutableState.EXPECT().GetInFlightDecision().Return(inFlightDecision, true).Times(1)
	s.mockMutableState.EXPECT().AddDecisionTaskFailedEvent(
		inFlightDecision.ScheduleID,
		inFlightDecision.StartedID,
		types.DecisionTaskFailedCauseFailoverCloseDecision,
		[]byte(nil),
		IdentityHistoryService,
		"",
		"",
		"",
		"",
		int64(0),
		"",
	).Return(&types.HistoryEvent{}, nil).Times(1)
	s.mockMutableState.EXPECT().FlushBufferedEvents().Return(nil).Times(1)

	s.mockMutableState.EXPECT().AddWorkflowExecutionTerminatedEvent(
		lastEventID+1, WorkflowTerminationReason, gomock.Any(), WorkflowTerminationIdentity,
	).Return(&types.HistoryEvent{}, nil).Times(1)

	// if workflow is in zombie or finished state, keep as is
	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(false).Times(1)
	policy, err := nDCWorkflow.SuppressBy(incomingNDCWorkflow)
	s.NoError(err)
	s.Equal(TransactionPolicyPassive, policy)

	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(true).Times(1)
	policy, err = nDCWorkflow.SuppressBy(incomingNDCWorkflow)
	s.NoError(err)
	s.Equal(TransactionPolicyActive, policy)
}

func (s *workflowSuite) TestSuppressWorkflowBy_Zombiefy() {
	lastEventTaskID := int64(144)
	lastEventVersion := cluster.TestAlternativeClusterInitialFailoverVersion
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastEventVersion, nil).AnyTimes()
	executionInfo := &persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           s.runID,
		LastEventTaskID: lastEventTaskID,
		State:           persistence.WorkflowStateRunning,
		CloseStatus:     persistence.WorkflowCloseStatusNone,
	}
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(executionInfo).AnyTimes()
	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)

	incomingRunID := uuid.New()
	incomingLastEventTaskID := int64(144)
	incomingLastEventVersion := lastEventVersion + 1
	incomingMockContext := NewMockContext(s.controller)
	incomingMockMutableState := NewMockMutableState(s.controller)
	incomingNDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		incomingMockContext,
		incomingMockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	incomingMockMutableState.EXPECT().GetLastWriteVersion().Return(incomingLastEventVersion, nil).AnyTimes()
	incomingMockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{
		DomainID:        s.domainID,
		WorkflowID:      s.workflowID,
		RunID:           incomingRunID,
		LastEventTaskID: incomingLastEventTaskID,
	}).AnyTimes()

	// if workflow is in zombie or finished state, keep as is
	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(false).Times(1)
	policy, err := nDCWorkflow.SuppressBy(incomingNDCWorkflow)
	s.NoError(err)
	s.Equal(TransactionPolicyPassive, policy)

	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(true).Times(1)
	policy, err = nDCWorkflow.SuppressBy(incomingNDCWorkflow)
	s.NoError(err)
	s.Equal(TransactionPolicyPassive, policy)
	s.Equal(persistence.WorkflowStateZombie, executionInfo.State)
	s.Equal(persistence.WorkflowCloseStatusNone, executionInfo.CloseStatus)
}

func (s *workflowSuite) TestRevive_Zombie_Error() {
	s.mockMutableState.EXPECT().GetWorkflowStateCloseStatus().Return(persistence.WorkflowStateZombie, persistence.WorkflowCloseStatusNone).Times(1)
	s.mockMutableState.EXPECT().HasProcessedOrPendingDecision().Return(true).Times(1)
	s.mockMutableState.EXPECT().UpdateWorkflowStateCloseStatus(persistence.WorkflowStateRunning, persistence.WorkflowCloseStatusNone).Return(&types.InternalServiceError{Message: "error"}).Times(1)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.Revive()
	s.Error(err)
}

func (s *workflowSuite) TestRevive_Zombie_Success() {
	s.mockMutableState.EXPECT().GetWorkflowStateCloseStatus().Return(persistence.WorkflowStateZombie, persistence.WorkflowCloseStatusNone).Times(1)
	s.mockMutableState.EXPECT().HasProcessedOrPendingDecision().Return(true).Times(1)
	s.mockMutableState.EXPECT().UpdateWorkflowStateCloseStatus(persistence.WorkflowStateRunning, persistence.WorkflowCloseStatusNone).Return(nil).Times(1)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.Revive()
	s.NoError(err)
}

func (s *workflowSuite) TestRevive_NonZombie_Success() {
	s.mockMutableState.EXPECT().GetWorkflowStateCloseStatus().Return(persistence.WorkflowStateCompleted, persistence.WorkflowCloseStatusNone).Times(1)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.Revive()
	s.NoError(err)
}

func (s *workflowSuite) TestFlushBufferedEvents_Success() {
	lastWriteVersion := cluster.TestCurrentClusterInitialFailoverVersion
	lastEventTaskID := int64(144)
	decision := &DecisionInfo{
		ScheduleID: 1,
		StartedID:  2,
	}

	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(true)
	s.mockMutableState.EXPECT().HasBufferedEvents().Return(true)
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastWriteVersion, nil)
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(&persistence.WorkflowExecutionInfo{LastEventTaskID: lastEventTaskID}).AnyTimes()
	s.mockMutableState.EXPECT().UpdateCurrentVersion(lastWriteVersion, true).Return(nil)
	s.mockMutableState.EXPECT().GetInFlightDecision().Return(decision, true)
	s.mockMutableState.EXPECT().AddDecisionTaskFailedEvent(decision.ScheduleID, decision.StartedID, types.DecisionTaskFailedCauseFailoverCloseDecision, nil, IdentityHistoryService, "", "", "", "", int64(0), "").Return(&types.HistoryEvent{}, nil)
	s.mockMutableState.EXPECT().FlushBufferedEvents().Return(nil)
	s.mockMutableState.EXPECT().HasPendingDecision().Return(false)
	s.mockMutableState.EXPECT().AddDecisionTaskScheduledEvent(false).Return(&DecisionInfo{}, nil)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.FlushBufferedEvents()
	s.NoError(err)
}

func (s *workflowSuite) TestFlushBufferedEvents_NoBuffer_Success() {
	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(true)
	s.mockMutableState.EXPECT().HasBufferedEvents().Return(false)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.FlushBufferedEvents()
	s.NoError(err)
}

func (s *workflowSuite) TestFlushBufferedEvents_NoDecision_Success() {
	lastWriteVersion := cluster.TestCurrentClusterInitialFailoverVersion
	lastEventTaskID := int64(144)

	s.mockMutableState.EXPECT().IsWorkflowExecutionRunning().Return(true)
	s.mockMutableState.EXPECT().HasBufferedEvents().Return(true)
	s.mockMutableState.EXPECT().GetLastWriteVersion().Return(lastWriteVersion, nil)
	s.mockMutableState.EXPECT().GetExecutionInfo().Return(
		&persistence.WorkflowExecutionInfo{
			DomainID:        s.domainID,
			WorkflowID:      s.workflowID,
			RunID:           s.runID,
			LastEventTaskID: lastEventTaskID,
		},
	).AnyTimes()
	s.mockMutableState.EXPECT().UpdateCurrentVersion(lastWriteVersion, true).Return(nil)
	s.mockMutableState.EXPECT().GetInFlightDecision().Return(nil, false)

	nDCWorkflow := NewWorkflow(
		context.Background(),
		cluster.TestActiveClusterMetadata,
		s.newTestActiveClusterManager(cluster.TestActiveClusterMetadata),
		s.mockContext,
		s.mockMutableState,
		NoopReleaseFn,
		testlogger.New(s.T()),
	)
	err := nDCWorkflow.FlushBufferedEvents()
	s.NoError(err)
}

func (s *workflowSuite) newTestActiveClusterManager(clusterMetadata cluster.Metadata) activecluster.Manager {
	domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
		return cache.NewGlobalDomainCacheEntryForTest(
			&persistence.DomainInfo{
				ID:   s.domainID,
				Name: s.domainName,
			},
			&persistence.DomainConfig{},
			&persistence.DomainReplicationConfig{
				ActiveClusterName: cluster.TestCurrentClusterName,
				Clusters: []*persistence.ClusterReplicationConfig{
					{ClusterName: cluster.TestCurrentClusterName},
					{ClusterName: cluster.TestAlternativeClusterName},
				},
			},
			clusterMetadata.GetAllClusterInfo()[cluster.TestCurrentClusterName].InitialFailoverVersion,
		), nil
	}

	// Create and return the active cluster manager
	activeClusterMgr, err := activecluster.NewManager(
		domainIDToDomainFn,
		clusterMetadata,
		nil,
		testlogger.New(s.T()),
		nil,
		nil,
		0,
	)
	if err != nil {
		s.T().Fatalf("failed to create active cluster manager, error: %v", err)
	}
	return activeClusterMgr
}

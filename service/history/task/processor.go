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

package task

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/exp/rand"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig/dynamicproperties"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/task"
	"github.com/uber/cadence/service/history/config"
)

type DomainPriorityKey struct {
	DomainID string
	Priority int
}

type processorImpl struct {
	sync.RWMutex

	priorityAssigner PriorityAssigner
	taskProcessor    task.Processor
	scheduler        task.Scheduler
	newScheduler     task.Scheduler

	status                    int32
	logger                    log.Logger
	metricsClient             metrics.Client
	timeSource                clock.TimeSource
	newSchedulerProbabilityFn dynamicproperties.IntPropertyFn
}

var (
	errTaskProcessorNotRunning = errors.New("queue task processor is not running")
)

// NewProcessor creates a new task processor
func NewProcessor(
	priorityAssigner PriorityAssigner,
	config *config.Config,
	logger log.Logger,
	metricsClient metrics.Client,
	timeSource clock.TimeSource,
	domainCache cache.DomainCache,
) (Processor, error) {
	taskProcessor := task.NewParallelTaskProcessor(
		logger,
		metricsClient,
		&task.ParallelTaskProcessorOptions{
			QueueSize:   1,
			WorkerCount: config.TaskSchedulerWorkerCount,
			RetryPolicy: common.CreateTaskProcessingRetryPolicy(),
		},
	)
	taskToChannelKeyFn := func(t task.PriorityTask) int {
		return t.Priority()
	}
	channelKeyToWeightFn := func(priority int) int {
		weights, err := dynamicproperties.ConvertDynamicConfigMapPropertyToIntMap(config.TaskSchedulerRoundRobinWeights())
		if err != nil {
			logger.Error("failed to convert dynamic config map to int map", tag.Error(err))
			weights = dynamicproperties.DefaultTaskSchedulerRoundRobinWeights
		}
		weight, ok := weights[priority]
		if !ok {
			logger.Error("weights not found for task priority", tag.Dynamic("priority", priority), tag.Dynamic("weights", weights))
		}
		return weight
	}
	options, err := task.NewSchedulerOptions[int](
		config.TaskSchedulerType(),
		config.TaskSchedulerQueueSize(),
		config.TaskSchedulerWorkerCount,
		config.TaskSchedulerDispatcherCount(),
		taskToChannelKeyFn,
		channelKeyToWeightFn,
	)
	if err != nil {
		return nil, err
	}
	scheduler, err := createTaskScheduler(options, logger, metricsClient, timeSource, taskProcessor)
	if err != nil {
		return nil, err
	}
	var newScheduler task.Scheduler
	var newSchedulerProbabilityFn dynamicproperties.IntPropertyFn
	if config.TaskSchedulerEnableMigration() {
		taskToChannelKeyFn := func(t task.PriorityTask) DomainPriorityKey {
			var domainID string
			tt, ok := t.(Task)
			if ok {
				domainID = tt.GetDomainID()
			} else {
				logger.Error("incorrect task type for task scheduler, this should not happen, there must be a bug in our code")
			}
			return DomainPriorityKey{
				DomainID: domainID,
				Priority: t.Priority(),
			}
		}
		channelKeyToWeightFn := func(k DomainPriorityKey) int {
			return getDomainPriorityWeight(logger, config, domainCache, k)
		}
		newScheduler, err = task.NewWeightedRoundRobinTaskScheduler(
			logger,
			metricsClient,
			timeSource,
			taskProcessor,
			&task.WeightedRoundRobinTaskSchedulerOptions[DomainPriorityKey]{
				QueueSize:            config.TaskSchedulerQueueSize(),
				DispatcherCount:      config.TaskSchedulerDispatcherCount(),
				TaskToChannelKeyFn:   taskToChannelKeyFn,
				ChannelKeyToWeightFn: channelKeyToWeightFn,
			},
		)
		if err != nil {
			return nil, err
		}
		newSchedulerProbabilityFn = config.TaskSchedulerMigrationRatio
	}
	return &processorImpl{
		priorityAssigner:          priorityAssigner,
		taskProcessor:             taskProcessor,
		scheduler:                 scheduler,
		newScheduler:              newScheduler,
		status:                    common.DaemonStatusInitialized,
		logger:                    logger,
		metricsClient:             metricsClient,
		timeSource:                timeSource,
		newSchedulerProbabilityFn: newSchedulerProbabilityFn,
	}, nil
}

func (p *processorImpl) Start() {
	if !atomic.CompareAndSwapInt32(&p.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	p.taskProcessor.Start()
	p.scheduler.Start()
	if p.newScheduler != nil {
		p.newScheduler.Start()
	}

	p.logger.Info("Queue task processor started.")
}

func (p *processorImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&p.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	if p.newScheduler != nil {
		p.newScheduler.Stop()
	}
	p.scheduler.Stop()
	p.taskProcessor.Stop()

	p.logger.Info("Queue task processor stopped.")
}

func (p *processorImpl) Submit(task Task) error {
	if err := p.priorityAssigner.Assign(task); err != nil {
		return err
	}
	if p.shouldUseNewScheduler() {
		p.metricsClient.IncCounter(metrics.HistoryTaskSchedulerMigrationScope, metrics.TaskRequestsNewScheduler)
		return p.newScheduler.Submit(task)
	}
	p.metricsClient.IncCounter(metrics.HistoryTaskSchedulerMigrationScope, metrics.TaskRequestsOldScheduler)
	return p.scheduler.Submit(task)
}

func (p *processorImpl) TrySubmit(task Task) (bool, error) {
	if err := p.priorityAssigner.Assign(task); err != nil {
		return false, err
	}
	if p.shouldUseNewScheduler() {
		p.metricsClient.IncCounter(metrics.HistoryTaskSchedulerMigrationScope, metrics.TaskRequestsNewScheduler)
		return p.newScheduler.TrySubmit(task)
	}
	p.metricsClient.IncCounter(metrics.HistoryTaskSchedulerMigrationScope, metrics.TaskRequestsOldScheduler)
	return p.scheduler.TrySubmit(task)
}

func (p *processorImpl) shouldUseNewScheduler() bool {
	if p.newScheduler == nil {
		return false
	}
	return rand.Intn(100) < p.newSchedulerProbabilityFn()
}

func createTaskScheduler(
	options *task.SchedulerOptions[int],
	logger log.Logger,
	metricsClient metrics.Client,
	timeSource clock.TimeSource,
	taskProcessor task.Processor,
) (task.Scheduler, error) {
	var scheduler task.Scheduler
	var err error
	switch options.SchedulerType {
	case task.SchedulerTypeFIFO:
		scheduler = task.NewFIFOTaskScheduler(
			logger,
			metricsClient,
			options.FIFOSchedulerOptions,
		)
	case task.SchedulerTypeWRR:
		scheduler, err = task.NewWeightedRoundRobinTaskScheduler(
			logger,
			metricsClient,
			timeSource,
			taskProcessor,
			options.WRRSchedulerOptions,
		)
	default:
		// the scheduler type has already been verified when initializing the processor
		panic(fmt.Sprintf("Unknown task scheduler type, %v", options.SchedulerType))
	}

	return scheduler, err
}

func getDomainPriorityWeight(
	logger log.Logger,
	config *config.Config,
	domainCache cache.DomainCache,
	k DomainPriorityKey,
) int {
	var weights map[int]int
	domainName, err := domainCache.GetDomainName(k.DomainID)
	if err != nil {
		logger.Error("failed to get domain name from cache, use default round robin weights", tag.Error(err))
		weights = dynamicproperties.DefaultTaskSchedulerRoundRobinWeights
	} else {
		weights, err = dynamicproperties.ConvertDynamicConfigMapPropertyToIntMap(config.TaskSchedulerDomainRoundRobinWeights(domainName))
		if err != nil {
			logger.Error("failed to convert dynamic config map to int map, use default round robin weights", tag.Error(err))
			weights = dynamicproperties.DefaultTaskSchedulerRoundRobinWeights
		}
	}
	weight, ok := weights[k.Priority]
	if !ok {
		logger.Error("weights not found for task priority, default to 1", tag.Dynamic("priority", k.Priority), tag.Dynamic("weights", weights))
		weight = 1
	}
	return weight
}

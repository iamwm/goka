package goka

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/lovoo/goka/logger"
	"github.com/lovoo/goka/multierr"
)

const (
	PPStateIdle State = iota
	PPStateRecovering
	PPStateRunning
	PPStateStopping
)

// PartitionProcessor handles message processing of one partition by serializing
// messages from different input topics.
// It also handles joined tables as well as lookup views (managed by `Processor`).
type PartitionProcessor struct {
	callbacks map[string]ProcessCallback

	log logger.Logger

	table   *PartitionTable
	joins   map[string]*PartitionTable
	lookups map[string]*View
	graph   *GroupGraph

	state *Signal

	partition int32

	input       chan *sarama.ConsumerMessage
	inputTopics []string

	runnerGroup       *multierr.ErrGroup
	cancelRunnerGroup func()

	consumer sarama.Consumer
	tmgr     TopicManager
	stats    *PartitionProcStats

	requestStats  chan bool
	responseStats chan *PartitionProcStats

	session  sarama.ConsumerGroupSession
	producer Producer

	opts *poptions
}

func newPartitionProcessor(partition int32,
	graph *GroupGraph,
	session sarama.ConsumerGroupSession,
	logger logger.Logger,
	opts *poptions,
	lookupTables map[string]*View,
	consumer sarama.Consumer,
	producer Producer,
	tmgr TopicManager) *PartitionProcessor {

	// collect all topics I am responsible for
	topicMap := make(map[string]bool)
	for _, stream := range graph.InputStreams() {
		topicMap[stream.Topic()] = true
	}
	if loop := graph.LoopStream(); loop != nil {
		topicMap[loop.Topic()] = true
	}

	var (
		topicList  []string
		outputList []string
		callbacks  = make(map[string]ProcessCallback)
	)
	for t := range topicMap {
		topicList = append(topicList, t)
		callbacks[t] = graph.callback(t)
	}
	for _, output := range graph.OutputStreams() {
		outputList = append(outputList, output.Topic())
	}
	if graph.LoopStream() != nil {
		outputList = append(outputList, graph.LoopStream().Topic())
	}

	if graph.GroupTable() != nil {
		outputList = append(outputList, graph.GroupTable().Topic())
	}

	log := logger.Prefix(fmt.Sprintf("PartitionProcessor (%d)", partition))

	partProc := &PartitionProcessor{
		log:           log,
		opts:          opts,
		partition:     partition,
		state:         NewSignal(PPStateIdle, PPStateRecovering, PPStateRunning, PPStateStopping).SetState(PPStateIdle),
		callbacks:     callbacks,
		lookups:       lookupTables,
		consumer:      consumer,
		producer:      producer,
		tmgr:          tmgr,
		joins:         make(map[string]*PartitionTable),
		input:         make(chan *sarama.ConsumerMessage, opts.partitionChannelSize),
		inputTopics:   topicList,
		graph:         graph,
		stats:         newPartitionProcStats(topicList, outputList),
		requestStats:  make(chan bool),
		responseStats: make(chan *PartitionProcStats, 1),
		session:       session,
	}

	if graph.GroupTable() != nil {
		partProc.table = newPartitionTable(graph.GroupTable().Topic(),
			partition,
			consumer,
			tmgr,
			opts.updateCallback,
			opts.builders.storage,
			log.Prefix("PartTable"),
		)
	}
	return partProc
}

func (pp *PartitionProcessor) EnqueueMessage(msg *sarama.ConsumerMessage) {
	pp.input <- msg
}

func (pp *PartitionProcessor) Recovered() bool {
	return pp.state.IsState(PPStateRunning)
}

func (pp *PartitionProcessor) Errors() <-chan error {
	errs := make(chan error)

	go func() {
		defer close(errs)
		err := pp.runnerGroup.Wait().NilOrError()
		if err != nil {
			errs <- err
		}
	}()
	return errs
}

func (pp *PartitionProcessor) Setup(ctx context.Context) error {
	ctx, pp.cancelRunnerGroup = context.WithCancel(ctx)

	var runnerCtx context.Context
	pp.runnerGroup, runnerCtx = multierr.NewErrGroup(ctx)

	setupErrg, setupCtx := multierr.NewErrGroup(ctx)

	pp.state.SetState(PPStateRecovering)
	defer pp.state.SetState(PPStateRunning)

	if pp.table != nil {
		setupErrg.Go(func() error {
			pp.log.Debugf("catching up table")
			defer pp.log.Debugf("catching up table done")
			return pp.table.SetupAndRecover(setupCtx)
		})
	}

	for _, join := range pp.graph.JointTables() {
		table := newPartitionTable(join.Topic(),
			pp.partition,
			pp.consumer,
			pp.tmgr,
			pp.opts.updateCallback,
			pp.opts.builders.storage,
			pp.log.Prefix(fmt.Sprintf("Join %s", join.Topic())),
		)
		pp.joins[join.Topic()] = table

		setupErrg.Go(func() error {
			table.SetupAndRecover(setupCtx)
			return nil
		})
	}

	// here we wait for our table and the joins to recover
	err := setupErrg.Wait().NilOrError()
	if err != nil {
		return fmt.Errorf("Setup failed. Cannot start processor for partition %d: %v", pp.partition, err)
	}

	// as the table is now recovered, we have to start handling stats requests
	// separately during running
	if pp.table != nil {
		pp.runnerGroup.Go(func() error {
			pp.table.handleStatsRequests(runnerCtx)
			return nil
		})
	}

	for _, join := range pp.joins {
		join := join
		pp.runnerGroup.Go(func() error {
			return join.CatchupForever(runnerCtx, false)
		})
	}

	// now run the processor in a runner-group
	pp.runnerGroup.Go(func() error {
		err := pp.run(runnerCtx)
		if err != nil {
			pp.log.Printf("Run failed with error: %v", err)
		}
		return err
	})
	return nil
}

// Stop stops the partition processor
func (pp *PartitionProcessor) Stop() error {
	pp.log.Debugf("Stopping")
	defer pp.log.Debugf("... Stopping done")
	pp.state.SetState(PPStateStopping)
	defer pp.state.SetState(PPStateIdle)
	errs := new(multierr.Errors)

	if pp.cancelRunnerGroup != nil {
		pp.cancelRunnerGroup()
	}
	if pp.runnerGroup != nil {
		errs.Collect(pp.runnerGroup.Wait().NilOrError())
	}
	if pp.table != nil {
		errs.Collect(pp.table.Close())
	}

	return errs.NilOrError()
}

func (pp *PartitionProcessor) run(ctx context.Context) (rerr error) {
	pp.log.Debugf("starting")
	defer pp.log.Debugf("stopped")

	errs := new(multierr.Errors)
	defer func() {
		errs.Collect(rerr)
		rerr = errs.NilOrError()
	}()

	var (
		// syncFailer is called synchronously from the callback within *this*
		// goroutine
		syncFailer = func(err error) {
			// only fail processor if context not already Done
			select {
			case <-ctx.Done():
				rerr = err
				return
			default:
			}
			panic(err)
		}

		closeOnce = new(sync.Once)
		asyncErrs = make(chan struct{})

		// asyncFailer is called asynchronously from other goroutines, e.g.
		// when the promise of a Emit (using a producer internally) fails
		asyncFailer = func(err error) {
			errs.Collect(err)
			closeOnce.Do(func() { close(asyncErrs) })
		}

		wg sync.WaitGroup
	)

	defer func() {
		if r := recover(); r != nil {
			rerr = fmt.Errorf("%v\n%v", r, string(debug.Stack()))
			return
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.NewTimer(60 * time.Second).C:
			pp.log.Printf("partition processor did not shutdown in time. Will stop waiting")
		}
	}()

	updateHwmStatsTicker := time.NewTicker(statsHwmUpdateInterval)
	defer updateHwmStatsTicker.Stop()

	for {
		select {
		case ev, isOpen := <-pp.input:
			// channel already closed, ev will be nil
			if !isOpen {
				return nil
			}
			err := pp.processMessage(ctx, &wg, ev, syncFailer, asyncFailer)
			if err != nil {
				return fmt.Errorf("error processing message: from %s %v", ev.Value, err)
			}

			pp.updateStats(ev)

		case <-pp.requestStats:
			stats := pp.collectStats(ctx)
			select {
			case pp.responseStats <- stats:
			case <-ctx.Done():
				pp.log.Debugf("exiting, context is cancelled")
				return
			}
		case <-updateHwmStatsTicker.C:
			pp.updateHwmStats()

		case <-ctx.Done():
			pp.log.Debugf("exiting, context is cancelled")
			return

		case <-asyncErrs:
			pp.log.Debugf("Errors occurred asynchronously. Will exit partition processor")
			return
		}
	}
}

func (pp *PartitionProcessor) updateStats(ev *sarama.ConsumerMessage) {
	ip := pp.stats.Input[ev.Topic]
	ip.Bytes += len(ev.Value)
	ip.LastOffset = ev.Offset
	if !ev.Timestamp.IsZero() {
		ip.Delay = time.Since(ev.Timestamp)
	}
	ip.Count++
}

func (pp *PartitionProcessor) updateHwmStats() {
	hwms := pp.consumer.HighWaterMarks()
	for input, inputStats := range pp.stats.Input {
		hwm := hwms[input][pp.partition]
		if hwm != 0 && inputStats.LastOffset != 0 {
			inputStats.OffsetLag = hwm - inputStats.LastOffset
		}
	}
}

func (pp *PartitionProcessor) collectStats(ctx context.Context) *PartitionProcStats {
	var (
		stats = pp.stats.clone()
		m     sync.Mutex
	)

	errg, ctx := multierr.NewErrGroup(ctx)

	for topic, join := range pp.joins {
		topic, join := topic, join
		errg.Go(func() error {
			joinStats := join.fetchStats(ctx)
			m.Lock()
			defer m.Unlock()
			stats.Joined[topic] = joinStats
			return nil
		})
	}

	if pp.table != nil {
		errg.Go(func() error {
			stats.TableStats = pp.table.fetchStats(ctx)
			return nil
		})
	}

	err := errg.Wait().NilOrError()
	if err != nil {
		pp.log.Printf("Error retrieving stats: %v", err)
	}

	return stats
}

func (pp *PartitionProcessor) fetchStats(ctx context.Context) *PartitionProcStats {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(fetchStatsTimeout):
		pp.log.Printf("requesting stats timed out")
		return nil
	case pp.requestStats <- true:
	}

	// retrieve from response-channel
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(fetchStatsTimeout):
		pp.log.Printf("Fetching stats timed out")
		return nil
	case stats := <-pp.responseStats:
		return stats
	}
}

func (pp *PartitionProcessor) processMessage(ctx context.Context, wg *sync.WaitGroup, msg *sarama.ConsumerMessage, syncFailer func(err error), asyncFailer func(err error)) error {
	msgContext := &cbContext{
		ctx:   ctx,
		graph: pp.graph,

		partProcStats: pp.stats,
		pviews:        pp.joins,
		views:         pp.lookups,
		commit:        func() { pp.session.MarkMessage(msg, "") },
		wg:            wg,
		msg:           msg,
		syncFailer:    syncFailer,
		asyncFailer:   asyncFailer,
		emitter:       pp.producer.Emit,
		table:         pp.table,
	}

	var (
		m   interface{}
		err error
	)

	// decide whether to decode or ignore message
	switch {
	case msg.Value == nil && pp.opts.nilHandling == NilIgnore:
		// mark the message upstream so we don't receive it again.
		// this is usually only an edge case in unit tests, as kafka probably never sends us nil messages
		pp.session.MarkMessage(msg, "")
		// otherwise drop it.
		return nil
	case msg.Value == nil && pp.opts.nilHandling == NilProcess:
		// process nil messages without decoding them
		m = nil
	default:
		// get stream subcription
		codec := pp.graph.codec(msg.Topic)
		if codec == nil {
			return fmt.Errorf("cannot handle topic %s", msg.Topic)
		}

		// decode message
		m, err = codec.Decode(msg.Value)
		if err != nil {
			return fmt.Errorf("error decoding message for key %s from %s/%d: %v", msg.Key, msg.Topic, msg.Partition, err)
		}
	}

	cb := pp.callbacks[msg.Topic]
	if cb == nil {
		return fmt.Errorf("error processing message for key %s from %s/%d: %v", string(msg.Key), msg.Topic, msg.Partition, err)
	}

	// start context and call the ProcessorCallback cb
	msgContext.start()

	// now call cb
	cb(msgContext, m)
	msgContext.finish(nil)
	return nil
}

package ethmonitor

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xsequence/ethkit/ethrpc"
	"github.com/0xsequence/ethkit/go-ethereum"
	"github.com/0xsequence/ethkit/go-ethereum/common"
	"github.com/0xsequence/ethkit/go-ethereum/core/types"
	"github.com/goware/channel"
	"github.com/goware/logger"
	"github.com/goware/superr"
)

var DefaultOptions = Options{
	Logger:                   logger.NewLogger(logger.LogLevel_WARN),
	PollingInterval:          1000 * time.Millisecond,
	Timeout:                  20 * time.Second,
	StartBlockNumber:         nil, // latest
	TrailNumBlocksBehindHead: 0,   // latest
	BlockRetentionLimit:      200,
	WithLogs:                 false,
	LogTopics:                []common.Hash{}, // all logs
	DebugLogging:             false,
}

type Options struct {
	// Logger used by ethmonitor to log warnings and debug info
	Logger logger.Logger

	// PollingInterval to query the chain for new blocks
	PollingInterval time.Duration

	// Timeout duration used by the rpc client when fetching data from the remote node.
	Timeout time.Duration

	// StartBlockNumber to begin the monitor from.
	StartBlockNumber *big.Int

	// Bootstrap flag which indicates the monitor will expect the monitor's
	// events to be bootstrapped, and will continue from that point. This als
	// takes precedence over StartBlockNumber when set to true.
	Bootstrap bool

	// TrailNumBlocksBehindHead is the number of blocks we trail behind
	// the head of the chain before broadcasting new events to the subscribers.
	TrailNumBlocksBehindHead int

	// BlockRetentionLimit is the number of blocks we keep on the canonical chain
	// cache.
	BlockRetentionLimit int

	// WithLogs will include logs with the blocks if specified true.
	WithLogs bool

	// LogTopics will filter only specific log topics to include.
	LogTopics []common.Hash

	// DebugLogging toggle
	DebugLogging bool
}

var (
	ErrFatal                 = errors.New("ethmonitor: fatal error, stopping")
	ErrReorg                 = errors.New("ethmonitor: block reorg")
	ErrUnexpectedParentHash  = errors.New("ethmonitor: unexpected parent hash")
	ErrUnexpectedBlockNumber = errors.New("ethmonitor: unexpected block number")
	ErrQueueFull             = errors.New("ethmonitor: publish queue is full")
	ErrMaxAttempts           = errors.New("ethmonitor: max attempts hit")
)

type Monitor struct {
	options Options

	log      logger.Logger
	provider *ethrpc.Provider

	chain           *Chain
	nextBlockNumber *big.Int

	publishCh    chan Blocks
	publishQueue *queue
	subscribers  []*subscriber

	ctx     context.Context
	ctxStop context.CancelFunc
	running int32
	mu      sync.RWMutex
}

func NewMonitor(provider *ethrpc.Provider, options ...Options) (*Monitor, error) {
	opts := DefaultOptions
	if len(options) > 0 {
		opts = options[0]
	}

	// TODO: in the future, consider using a multi-provider, and querying data from multiple
	// sources to ensure all matches. we could build this directly inside of ethrpc too

	// TODO: lets see if we can use ethrpc websocket for this set of data

	if opts.Logger == nil {
		return nil, fmt.Errorf("ethmonitor: logger is nil")
	}

	opts.BlockRetentionLimit += opts.TrailNumBlocksBehindHead

	if opts.DebugLogging {
		stdLogger, ok := opts.Logger.(*logger.StdLogAdapter)
		if ok {
			stdLogger.Level = logger.LogLevel_DEBUG
		}
	}

	return &Monitor{
		options:      opts,
		log:          opts.Logger,
		provider:     provider,
		chain:        newChain(opts.BlockRetentionLimit, opts.Bootstrap),
		publishCh:    make(chan Blocks),
		publishQueue: newQueue(opts.BlockRetentionLimit * 2),
		subscribers:  make([]*subscriber, 0),
	}, nil
}

func (m *Monitor) Run(ctx context.Context) error {
	if m.IsRunning() {
		return fmt.Errorf("ethmonitor: already running")
	}

	m.ctx, m.ctxStop = context.WithCancel(ctx)

	atomic.StoreInt32(&m.running, 1)
	defer atomic.StoreInt32(&m.running, 0)

	// Check if in bootstrap mode -- in which case we expect nextBlockNumber
	// to already be set.
	if m.options.Bootstrap && m.chain.blocks == nil {
		return errors.New("ethmonitor: monitor is in Bootstrap mode, and must be bootstrapped before run")
	}

	// Start from latest, or start from a specific block number
	if m.chain.Head() != nil {
		// starting from last block of our canonical chain
		m.nextBlockNumber = big.NewInt(0).Add(m.chain.Head().Number(), big.NewInt(1))
	} else if m.options.StartBlockNumber != nil {
		if m.options.StartBlockNumber.Cmp(big.NewInt(0)) >= 0 {
			// starting from specific block number
			m.nextBlockNumber = m.options.StartBlockNumber
		} else {
			// starting some number blocks behind the latest block num
			latestBlock, _ := m.provider.BlockByNumber(m.ctx, nil)
			if latestBlock != nil && latestBlock.Number() != nil {
				m.nextBlockNumber = big.NewInt(0).Add(latestBlock.Number(), m.options.StartBlockNumber)
				if m.nextBlockNumber.Cmp(big.NewInt(0)) < 0 {
					m.nextBlockNumber = nil
				}
			}
		}
	} else {
		// noop, starting from the latest block on the network
	}

	if m.nextBlockNumber == nil {
		m.log.Info("ethmonitor: starting from block=latest")
	} else {
		m.log.Infof("ethmonitor: starting from block=%d", m.nextBlockNumber)
	}

	// Broadcast published events to all subscribers
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case blocks := <-m.publishCh:
				if m.options.DebugLogging {
					m.log.Debug("ethmonitor: publishing block", blocks.LatestBlock().NumberU64(), "# events:", len(blocks))
				}

				// broadcast to subscribers
				m.broadcast(blocks)
			}
		}
	}()

	// Monitor the chain for canonical representation
	return m.monitor()
}

func (m *Monitor) Stop() {
	m.log.Info("ethmonitor: stop")
	m.ctxStop()
}

func (m *Monitor) IsRunning() bool {
	return atomic.LoadInt32(&m.running) == 1
}

func (m *Monitor) Options() Options {
	return m.options
}

func (m *Monitor) Provider() *ethrpc.Provider {
	return m.provider
}

func (m *Monitor) monitor() error {
	ctx := m.ctx
	events := Blocks{}

	// pollInterval is used for adaptive interval
	pollInterval := m.options.PollingInterval

	// monitor run loop
	for {
		select {

		case <-m.ctx.Done():
			return nil

		case <-time.After(pollInterval):
			headBlock := m.chain.Head()
			if headBlock != nil {
				m.nextBlockNumber = big.NewInt(0).Add(headBlock.Number(), big.NewInt(1))
			}

			nextBlock, err := m.fetchBlockByNumber(ctx, m.nextBlockNumber)
			if err == ethereum.NotFound {
				// reset poll interval as by config
				pollInterval = m.options.PollingInterval
				continue
			}
			if err != nil {
				m.log.Warnf("ethmonitor: [retrying] failed to fetch next block # %d, due to: %v", m.nextBlockNumber, err)
				pollInterval = m.options.PollingInterval // reset poll interval
				continue
			}

			// speed up the poll interval if we found the next block
			pollInterval /= 2

			// build deterministic set of add/remove events which construct the canonical chain
			events, err = m.buildCanonicalChain(ctx, nextBlock, events)
			if err != nil {
				m.log.Warnf("ethmonitor: error reported '%v', failed to build chain for next blockNum:%d blockHash:%s, retrying..",
					err, nextBlock.NumberU64(), nextBlock.Hash().Hex())

				// pause, then retry
				time.Sleep(m.options.PollingInterval)
				continue
			}

			if m.options.WithLogs {
				m.addLogs(ctx, events)
				m.backfillChainLogs(ctx)
			} else {
				for _, b := range events {
					b.Logs = nil // nil it out to be clear to subscribers
					b.OK = true
				}
			}

			// publish events
			err = m.publish(ctx, events)
			if err != nil {
				// failing to publish is considered a rare, but fatal error.
				// the only time this happens is if we fail to push an event to the publish queue.
				return superr.New(ErrFatal, err)
			}

			// clear events sink
			events = Blocks{}
		}
	}
}

func (m *Monitor) buildCanonicalChain(ctx context.Context, nextBlock *types.Block, events Blocks) (Blocks, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	headBlock := m.chain.Head()

	m.log.Debugf("ethmonitor: new block #%d hash:%s prevHash:%s numTxns:%d",
		nextBlock.NumberU64(), nextBlock.Hash().String(), nextBlock.ParentHash().String(), len(nextBlock.Transactions()))

	if headBlock == nil || nextBlock.ParentHash() == headBlock.Hash() {
		// block-chaining it up
		block := &Block{Event: Added, Block: nextBlock}
		events = append(events, block)
		return events, m.chain.push(block)
	}

	// next block doest match prevHash, therefore we must pop our previous block and recursively
	// rebuild the canonical chain
	poppedBlock := *m.chain.pop() // assign by value so it won't be mutated later
	poppedBlock.Event = Removed
	poppedBlock.OK = true // removed blocks are ready

	m.log.Debugf("ethmonitor: block reorg, reverting block #%d hash:%s prevHash:%s", poppedBlock.NumberU64(), poppedBlock.Hash().Hex(), poppedBlock.ParentHash().Hex())
	events = append(events, &poppedBlock)

	// let's always take a pause between any reorg for the polling interval time
	// to allow nodes to sync to the correct chain
	pause := m.options.PollingInterval * time.Duration(len(events))
	time.Sleep(pause)

	// Fetch/connect the broken chain backwards by traversing recursively via parent hashes
	nextParentBlock, err := m.fetchBlockByHash(ctx, nextBlock.ParentHash())
	if err != nil {
		// NOTE: this is okay, it will auto-retry
		return events, err
	}

	events, err = m.buildCanonicalChain(ctx, nextParentBlock, events)
	if err != nil {
		// NOTE: this is okay, it will auto-retry
		return events, err
	}

	block := &Block{Event: Added, Block: nextBlock}
	err = m.chain.push(block)
	if err != nil {
		return events, err
	}
	events = append(events, block)

	return events, nil
}

func (m *Monitor) addLogs(ctx context.Context, blocks Blocks) {
	tctx, cancel := context.WithTimeout(ctx, m.options.Timeout)
	defer cancel()

	for _, block := range blocks {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// skip, we already have logs for this block or its a removed block
		if block.OK {
			continue
		}

		// do not attempt to get logs for re-org'd blocks as the data
		// will be inconsistent and may never be available.
		if block.Event == Removed {
			block.OK = true
			continue
		}

		blockHash := block.Hash()

		topics := [][]common.Hash{}
		if len(m.options.LogTopics) > 0 {
			topics = append(topics, m.options.LogTopics)
		}

		logs, err := m.provider.FilterLogs(tctx, ethereum.FilterQuery{
			BlockHash: &blockHash,
			Topics:    topics,
		})

		if err == nil {
			// check the logsBloom from the block to check if we should be expecting logs. logsBloom
			// will be included for any indexed logs.
			if len(logs) > 0 || block.Bloom() == (types.Bloom{}) {
				// successful backfill
				if logs == nil {
					block.Logs = []types.Log{}
				} else {
					block.Logs = logs
				}
				block.OK = true
				continue
			}
		}

		// mark for backfilling
		block.Logs = nil
		block.OK = false

		// NOTE: we do not error here as these logs will be backfilled before they are published anyways,
		// but we log the error anyways.
		m.log.Infof("ethmonitor: [getLogs failed -- marking block %s for log backfilling] %v", blockHash.Hex(), err)
	}
}

func (m *Monitor) backfillChainLogs(ctx context.Context) {
	// Backfill logs for failed getLog calls across the retained chain.

	// In cases of re-orgs and inconsistencies with node state, in certain cases
	// we have to backfill log fetching and send an updated block event to subscribers.

	// We start by looking through our entire blocks retention for addLogs failed
	// and attempt to fetch the logs again for the same block object.
	//
	// NOTE: we only back-fill 'Added' blocks, as any 'Removed' blocks could be reverted
	// and their logs will never be available from a node.
	blocks := m.chain.Blocks()

	for i := len(blocks) - 1; i >= 0; i-- {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !blocks[i].OK {
			m.addLogs(ctx, Blocks{blocks[i]})
			if blocks[i].Event == Added && blocks[i].OK {
				m.log.Infof("ethmonitor: [getLogs backfill successful for block:%d %s]", blocks[i].NumberU64(), blocks[i].Hash().Hex())
			}
		}
	}
}

func (m *Monitor) fetchBlockByNumber(ctx context.Context, num *big.Int) (*types.Block, error) {
	maxErrAttempts, errAttempts := 10, 0 // in case of node connection failures

	var block *types.Block
	var err error

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if errAttempts >= maxErrAttempts {
			m.log.Warnf("ethmonitor: fetchBlockByNumber hit maxErrAttempts after %d tries for block num %v due to %v", errAttempts, num, err)
			return nil, superr.New(ErrMaxAttempts, err)
		}

		tctx, cancel := context.WithTimeout(ctx, m.options.Timeout)
		defer cancel()

		block, err = m.provider.BlockByNumber(tctx, num)
		if err != nil {
			if err == ethereum.NotFound {
				return nil, ethereum.NotFound
			} else {
				m.log.Warnf("ethmonitor: fetchBlockByNumber failed due to: %v", err)
				errAttempts++
				time.Sleep(m.options.PollingInterval * time.Duration(errAttempts) * 2)
				continue
			}
		}
		return block, nil
	}
}

func (m *Monitor) fetchBlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	maxNotFoundAttempts, notFoundAttempts := 4, 0 // waiting for node to sync
	maxErrAttempts, errAttempts := 10, 0          // in case of node connection failures

	var block *types.Block
	var err error

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if notFoundAttempts >= maxNotFoundAttempts {
			return nil, ethereum.NotFound
		}
		if errAttempts >= maxErrAttempts {
			m.log.Warnf("ethmonitor: fetchBlockByHash hit maxErrAttempts after %d tries for block hash %s due to %v", errAttempts, hash.Hex(), err)
			return nil, superr.New(ErrMaxAttempts, err)
		}

		block, err = m.provider.BlockByHash(ctx, hash)
		if err != nil {
			if err == ethereum.NotFound {
				notFoundAttempts++
				time.Sleep(m.options.PollingInterval * time.Duration(notFoundAttempts) * 2)
				continue
			} else {
				errAttempts++
				time.Sleep(m.options.PollingInterval * time.Duration(errAttempts) * 2)
				continue
			}
		}
		if block != nil {
			return block, nil
		}
	}
}

func (m *Monitor) publish(ctx context.Context, events Blocks) error {
	// Check for trail-behind-head mode and set maxBlockNum if applicable
	maxBlockNum := uint64(0)
	if m.options.TrailNumBlocksBehindHead > 0 {
		maxBlockNum = m.LatestBlock().NumberU64() - uint64(m.options.TrailNumBlocksBehindHead)
	}

	// Enqueue
	err := m.publishQueue.enqueue(events)
	if err != nil {
		return err
	}

	// Publish events existing in the queue
	pubEvents, ok := m.publishQueue.dequeue(maxBlockNum)
	if ok {
		m.publishCh <- pubEvents
	}

	return nil
}

func (m *Monitor) broadcast(events Blocks) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sub := range m.subscribers {
		sub.ch.Send(events)
	}
}

func (m *Monitor) Subscribe() Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()

	subscriber := &subscriber{
		ch:   channel.NewUnboundedChan[Blocks](m.log, 100, 5000),
		done: make(chan struct{}),
	}

	subscriber.unsubscribe = func() {
		close(subscriber.done)
		subscriber.ch.Close()
		subscriber.ch.Flush()

		m.mu.Lock()
		defer m.mu.Unlock()

		for i, sub := range m.subscribers {
			if sub == subscriber {
				m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
				return
			}
		}
	}

	m.subscribers = append(m.subscribers, subscriber)

	return subscriber
}

func (m *Monitor) Chain() *Chain {
	return m.chain
}

// LatestBlock will return the head block of the canonical chain
func (m *Monitor) LatestBlock() *Block {
	return m.chain.Head()
}

// LatestBlockNum returns the latest block number in the canonical chain
func (m *Monitor) LatestBlockNum() *big.Int {
	latestBlock := m.LatestBlock()
	if latestBlock == nil {
		return big.NewInt(0)
	} else {
		return big.NewInt(0).Set(latestBlock.Number())
	}
}

// LatestFinalBlock returns the latest block which has reached finality.
// The argument `numBlocksToFinality` should be a constant value of the number
// of blocks a particular chain needs to reach finality. Ie. on Polygon this
// value would be 120 and on Ethereum it would be 20. As the pubsub system
// publishes new blocks, this value will change, as the chain will progress
// forward. It's recommend / safe to call this method each time in a <-sub.Blocks()
// code block.
func (m *Monitor) LatestFinalBlock(numBlocksToFinality int) *Block {
	m.chain.mu.Lock()
	defer m.chain.mu.Unlock()

	n := len(m.chain.blocks)
	if n < numBlocksToFinality+1 {
		// not enough blocks have been monitored yet
		return nil
	} else {
		// return the block at finality position from the canonical chain
		return m.chain.blocks[n-numBlocksToFinality-1]
	}
}

func (m *Monitor) OldestBlockNum() *big.Int {
	oldestBlock := m.chain.Tail()
	if oldestBlock == nil {
		return big.NewInt(0)
	} else {
		return big.NewInt(0).Set(oldestBlock.Number())
	}
}

// GetBlock will search the retained blocks for the hash
func (m *Monitor) GetBlock(blockHash common.Hash) *Block {
	return m.chain.GetBlock(blockHash)
}

// GetBlock will search within the retained canonical chain for the txn hash. Passing `optMined true`
// will only return transaction which have not been removed from the chain via a reorg.
func (m *Monitor) GetTransaction(txnHash common.Hash) *types.Transaction {
	return m.chain.GetTransaction(txnHash)
}

// GetAverageBlockTime returns the average block time in seconds (including fractions)
func (m *Monitor) GetAverageBlockTime() float64 {
	return m.chain.GetAverageBlockTime()
}

// PurgeHistory clears all but the head of the chain. Useful for tests, but should almost
// never be used in a normal application.
func (m *Monitor) PurgeHistory() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.chain.blocks) > 1 {
		m.chain.mu.Lock()
		defer m.chain.mu.Unlock()
		m.chain.blocks = m.chain.blocks[1:1]
	}
}

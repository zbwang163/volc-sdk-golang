package producer

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/volcengine/volc-sdk-golang/service/tls/pb"
)

const (
	batchQueueSize = 100
)

type BatchKey struct {
	Topic       string
	Source      string
	ShardHash   string
	FileName    string
	CallBackFun CallBack
}

type BatchLog struct {
	Key BatchKey
	Log *pb.Log
}

type Dispatcher struct {
	stopCh         chan struct{}
	forceQuitCh    chan struct{}
	newLogRecvChan chan *BatchLog
	retryQueue     *RetryQueue
	lock           *sync.RWMutex
	logGroupData   map[string]*Batch
	producerConfig *Config
	sender         *Sender
	logger         log.Logger
	threadPool     *ThreadPool
	producer       *producer
}

func initDispatcher(config *Config, sender *Sender, logger log.Logger, threadPool *ThreadPool, producer *producer) *Dispatcher {
	return &Dispatcher{
		stopCh:         make(chan struct{}),
		forceQuitCh:    make(chan struct{}),
		newLogRecvChan: make(chan *BatchLog, batchQueueSize),
		logGroupData:   make(map[string]*Batch),
		retryQueue:     sender.retryQueue,
		lock:           &sync.RWMutex{},
		producerConfig: config,
		sender:         sender,
		logger:         logger,
		threadPool:     threadPool,
		producer:       producer,
	}
}

func (dispatcher *Dispatcher) addOrSendProducerBatch(key string, batchLog *BatchLog, producerBatch *Batch) {
	totalDataCount := producerBatch.getLogCount() + 1

	if producerBatch.totalDataSize > dispatcher.producerConfig.MaxBatchSize && producerBatch.totalDataSize < 11*1024*1024 && totalDataCount <= dispatcher.producerConfig.MaxBatchCount {
		producerBatch.addLogToLogGroup(batchLog.Log)
		if batchLog.Key.CallBackFun != nil {
			producerBatch.addProducerBatchCallBack(batchLog.Key.CallBackFun)
		}
		dispatcher.innerSendToServer(key, producerBatch)
	} else if producerBatch.totalDataSize <= dispatcher.producerConfig.MaxBatchSize && totalDataCount <= dispatcher.producerConfig.MaxBatchCount {
		producerBatch.addLogToLogGroup(batchLog.Log)
		if batchLog.Key.CallBackFun != nil {
			producerBatch.addProducerBatchCallBack(batchLog.Key.CallBackFun)
		}
	} else {
		dispatcher.innerSendToServer(key, producerBatch)
		dispatcher.createNewProducerBatch(batchLog, key)
	}
}

func (dispatcher *Dispatcher) run(dispatcherWaitGroup *sync.WaitGroup) {
	defer dispatcherWaitGroup.Done()

	for {
		select {
		case newBatchLog := <-dispatcher.newLogRecvChan:
			dispatcher.handleLogs(newBatchLog)
		case <-dispatcher.stopCh:
			// let the background checker to send the rest producerBatches
			return
		case <-dispatcher.forceQuitCh:
			return
		}
	}
}

func (dispatcher *Dispatcher) checkBatches(config *Config) {
	sleepMs := config.LingerTime

	dispatcher.lock.Lock()
	mapCount := len(dispatcher.logGroupData)
	for key, batch := range dispatcher.logGroupData {
		timeInterval := time.Since(batch.createTime)
		if timeInterval >= config.LingerTime {
			level.Debug(dispatcher.logger).Log("msg", "mover goroutine execute sent producerBatch to Sender")
			dispatcher.threadPool.taskChan <- batch
			delete(dispatcher.logGroupData, key)
		} else {
			if sleepMs > timeInterval {
				sleepMs = timeInterval
			}
		}
	}
	dispatcher.lock.Unlock()

	if mapCount == 0 {
		level.Debug(dispatcher.logger).Log("msg", "No data time in map waiting for user configured RemainMs parameter values")
		sleepMs = config.LingerTime
	}

	retryProducerBatchList := dispatcher.retryQueue.getRetryBatch(false)
	if retryProducerBatchList == nil {
		// If there is nothing to send in the retry queue, just wait for the minimum time that was given to me last time.
		time.Sleep(sleepMs)
	} else {
		for _, producerBatch := range retryProducerBatchList {
			dispatcher.threadPool.taskChan <- producerBatch
		}
	}
}

func (dispatcher *Dispatcher) checkBatchTime(dispatcherWaitGroup *sync.WaitGroup, config *Config) {
	defer dispatcherWaitGroup.Done()
	for {
		select {
		case <-dispatcher.stopCh:
			dispatcher.RetryQueueElegantQuit()
			return
		case <-dispatcher.forceQuitCh:
			return
		default:
			dispatcher.checkBatches(config)
		}
	}
}

func (dispatcher *Dispatcher) RetryQueueElegantQuit() {
	close(dispatcher.newLogRecvChan)
	for batchLog := range dispatcher.newLogRecvChan {
		dispatcher.handleLogs(batchLog)
	}

	dispatcher.lock.Lock()
	for _, batch := range dispatcher.logGroupData {
		dispatcher.threadPool.taskChan <- batch
	}

	dispatcher.logGroupData = make(map[string]*Batch)
	dispatcher.lock.Unlock()

	// pop all retry batches
	producerBatchList := dispatcher.retryQueue.getRetryBatch(true)
	for _, producerBatch := range producerBatchList {
		dispatcher.threadPool.taskChan <- producerBatch
	}
}

func (dispatcher *Dispatcher) handleLogs(batchLog *BatchLog) {
	if batchLog == nil {
		// dispatcher is closed
		return
	}

	key := dispatcher.getKeyString(batchLog.Key)
	dispatcher.lock.Lock()

	logSize := int64(GetLogSize(batchLog.Log))
	if producerBatch, ok := dispatcher.logGroupData[key]; ok == true {
		atomic.AddInt64(&producerBatch.totalDataSize, logSize)
		atomic.AddInt64(&dispatcher.producer.producerLogGroupSize, logSize)

		dispatcher.addOrSendProducerBatch(key, batchLog, producerBatch)
	} else {
		dispatcher.createNewProducerBatch(batchLog, key)
		atomic.AddInt64(&dispatcher.logGroupData[key].totalDataSize, logSize)
		atomic.AddInt64(&dispatcher.producer.producerLogGroupSize, logSize)
	}

	dispatcher.lock.Unlock()
}

func (dispatcher *Dispatcher) createNewProducerBatch(batchLog *BatchLog, key string) {
	level.Debug(dispatcher.logger).Log("msg", "Create a new ProducerBatch")

	newProducerBatch := initProducerBatch(batchLog, dispatcher.producerConfig)

	dispatcher.logGroupData[key] = newProducerBatch
}

func (dispatcher *Dispatcher) innerSendToServer(key string, producerBatch *Batch) {
	level.Debug(dispatcher.logger).Log("msg", "Send producerBatch to Sender from dispatcher")

	dispatcher.threadPool.taskChan <- producerBatch

	delete(dispatcher.logGroupData, key)
}

func (dispatcher *Dispatcher) getKeyString(batchKey BatchKey) string {
	var key strings.Builder

	key.WriteString(batchKey.Topic)
	key.WriteString(delimiter)
	key.WriteString(batchKey.ShardHash)
	key.WriteString(delimiter)
	key.WriteString(batchKey.Source)
	key.WriteString(delimiter)
	key.WriteString(batchKey.FileName)

	return key.String()
}

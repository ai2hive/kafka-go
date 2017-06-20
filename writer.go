package kafka

import (
	"context"
	"io"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// The Writer type provides the implementation of a producer of kafka messages
// that automatically distributes messages across partitions of a single topic
// using a configurable blancing policy.
//
// Instances of Writer are safe to use concurrently from multiple goroutines.
type Writer struct {
	config WriterConfig

	mutex  sync.RWMutex
	closed bool

	join sync.WaitGroup
	msgs chan writerMessage
	done chan struct{}
}

// WriterConfig is a configuration type used to create new instances of Writer.
type WriterConfig struct {
	// The list of brokers used to discover the partitions available on the
	// kafka cluster.
	//
	// This field is required, attempting to create a writer with an empty list
	// of brokers will panic.
	Brokers []string

	// The topic that the writer will produce messages to.
	//
	// This field is required, attempting to create a writer with an empty topic
	// will panic.
	Topic string

	// The dialer used by the writer to establish connections to the kafka
	// cluster.
	//
	// If nil, the default dialer is used instead.
	Dialer *Dialer

	// The balancer used to distribute messages across partitions.
	//
	// The default is to use a round-robin distribution.
	Balancer Balancer

	// Limit on how many attempts will be made to deliver a message.
	//
	// The default is to try at most 10 times.
	MaxAttempts int

	// A hint on the capacity of the writer's internal messager queue.
	//
	// The default is to use a queue capacity of 100 messages.
	QueueCapacity int

	// Limit on how many messages will be buffered before being sent to a
	// partition.
	//
	// The default is to use a target batch size of 100 messages.
	BatchSize int

	// Time limit on how often incomplete message batches will be flushed to
	// kafka.
	//
	// The default is to flush at least every second.
	BatchTimeout time.Duration

	// Timeout for read operations performed by the Writer.
	//
	// Defaults to 10 seconds.
	ReadTimeout time.Duration

	// Timeout for write operation performed by the Writer.
	//
	// Defaults to 10 seconds.
	WriteTimeout time.Duration

	// This interval defines how often the list of partitions is refreshed from
	// kafka. It allows the writer to automatically handle when new partitions
	// are added to a topic.
	//
	// The default is to refresh partitions every 15 seconds.
	RebalanceInterval time.Duration

	// Setting this flag to true causes the WriteMessages method to never block.
	// It also means that errors are ignored since the caller will not receive
	// the returned value. Use this only if you don't care about garantees of
	// whether the messages were written to kafka.
	Async bool
}

// NewWriter creates and returns a new Writer configured with config.
func NewWriter(config WriterConfig) *Writer {
	if len(config.Brokers) == 0 {
		panic("cannot create a kafka writer with an empty list of brokers")
	}

	if len(config.Topic) == 0 {
		panic("cannot create a kafak writer with an empty topic")
	}

	if config.Dialer == nil {
		config.Dialer = DefaultDialer
	}

	if config.Balancer == nil {
		config.Balancer = &RoundRobin{}
	}

	if config.MaxAttempts == 0 {
		config.MaxAttempts = 10
	}

	if config.QueueCapacity == 0 {
		config.QueueCapacity = 100
	}

	if config.BatchSize == 0 {
		config.BatchSize = 100
	}

	if config.BatchTimeout == 0 {
		config.BatchTimeout = 1 * time.Second
	}

	if config.ReadTimeout == 0 {
		config.ReadTimeout = 10 * time.Second
	}

	if config.WriteTimeout == 0 {
		config.WriteTimeout = 10 * time.Second
	}

	if config.RebalanceInterval == 0 {
		config.RebalanceInterval = 15 * time.Second
	}

	w := &Writer{
		config: config,
		msgs:   make(chan writerMessage, config.QueueCapacity),
		done:   make(chan struct{}),
	}

	w.join.Add(1)
	go w.run()
	return w
}

// WriteMessages writes a batch of messages to the kafka topic configured on this
// writer.
//
// Unless the writer was configured to write messages asynchronously, the method
// blocks until all messages have been written, or until the maximum number of
// attempts was reached.
//
// When the method returns an error, there's no way to know yet which messages
// have succeeded of failed.
//
// The context passed as first argument may also be used to asynchronously
// cancel the operation. Note that in this case there are no garantees made on
// whether messages were written to kafka. The program should assume that the
// whole batch failed and re-write the messages later (which could then cause
// duplicates).
func (w *Writer) WriteMessages(ctx context.Context, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}

	var res = make(chan error, len(msgs))
	var err error

	for attempt := 0; attempt != w.config.MaxAttempts; attempt++ {
		w.mutex.RLock()

		if w.closed {
			w.mutex.RUnlock()
			return io.ErrClosedPipe
		}

		for _, msg := range msgs {
			select {
			case w.msgs <- writerMessage{
				msg: msg,
				res: res,
			}:
			case <-ctx.Done():
				w.mutex.RUnlock()
				return ctx.Err()
			}
		}

		w.mutex.RUnlock()

		if w.config.Async {
			break
		}

		var retry []Message
		for i := 0; i != len(msgs); i++ {
			select {
			case e := <-res:
				if e != nil {
					if we, ok := e.(*writerError); ok {
						retry, err = append(retry, we.msg), we.err
					} else {
						err = e
					}
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if msgs = retry; len(msgs) == 0 {
			break
		}

		timer := time.NewTimer(backoff(attempt+1, 100*time.Millisecond, 1*time.Second))
		select {
		case <-timer.C:
			err = nil
		case <-ctx.Done():
			err = ctx.Err()
		case <-w.done:
			err = io.ErrClosedPipe
		}
		timer.Stop()

		if err != nil {
			break
		}
	}

	return err
}

// Close flushes all buffered messages and closes the writer. The call to Close
// aborts any concurrent calls to WriteMessages, which then return with the
// io.ErrClosedPipe error.
func (w *Writer) Close() (err error) {
	w.mutex.Lock()

	if !w.closed {
		w.closed = true
		close(w.msgs)
		close(w.done)
	}

	w.mutex.Unlock()
	w.join.Wait()
	return
}

func (w *Writer) run() {
	defer w.join.Done()

	ticker := time.NewTicker(w.config.RebalanceInterval)
	defer ticker.Stop()

	var rebalance = true
	var writers = make(map[int]*writer)
	var partitions []int
	var err error

	for {
		if rebalance {
			rebalance = false

			var newPartitions []int
			var oldPartitions = partitions

			if newPartitions, err = w.partitions(); err == nil {
				for _, partition := range diffp(oldPartitions, newPartitions) {
					w.close(writers[partition])
					delete(writers, partition)
				}

				for _, partition := range diffp(newPartitions, oldPartitions) {
					writers[partition] = w.open(partition)
				}

				partitions = newPartitions
			}
		}

		select {
		case wm, ok := <-w.msgs:
			if !ok {
				for _, writer := range writers {
					w.close(writer)
				}
				return
			}

			if len(partitions) != 0 {
				writers[w.config.Balancer.Balance(wm.msg, partitions...)].msgs <- wm
			} else {
				wm.res <- err
			}

		case <-ticker.C:
			rebalance = true
		}
	}
}

func (w *Writer) partitions() (partitions []int, err error) {
	for _, broker := range shuffledStrings(w.config.Brokers) {
		var conn *Conn
		var plist []Partition

		ctx, cancel := context.WithTimeout(context.Background(), w.config.ReadTimeout)

		if conn, err = w.config.Dialer.DialContext(ctx, "tcp", broker); err != nil {
			cancel()
			continue
		}

		deadline, _ := ctx.Deadline()
		cancel()
		conn.SetReadDeadline(deadline)

		plist, err = conn.ReadPartitions(w.config.Topic)
		conn.Close()

		if err == nil {
			partitions = make([]int, len(plist))
			for i, p := range plist {
				partitions[i] = p.ID
			}
			break
		}
	}

	sort.Ints(partitions)
	return
}

func (w *Writer) open(partition int) (writer *writer) {
	writer = newWriter(partition, w.config)
	w.join.Add(1)

	go func() {
		defer w.join.Done()
		writer.run()
	}()

	return writer
}

func (w *Writer) close(writer *writer) {
	writer.close()
	w.join.Add(1)

	go func() {
		defer w.join.Done()
		w.mutex.RLock()

		if w.closed {
			for wm := range writer.msgs {
				wm.res <- io.ErrClosedPipe
			}
		} else {
			for wm := range writer.msgs {
				w.msgs <- wm
			}
		}

		w.mutex.RUnlock()
	}()
}

func diffp(new []int, old []int) (diff []int) {
	for _, p := range new {
		if i := sort.SearchInts(old, p); i == len(old) || old[i] != p {
			diff = append(diff, p)
		}
	}
	return
}

type writer struct {
	brokers      []string
	topic        string
	partition    int
	batchSize    int
	batchTimeout time.Duration
	writeTimeout time.Duration
	dialer       *Dialer
	msgs         chan writerMessage
}

func newWriter(partition int, config WriterConfig) *writer {
	return &writer{
		brokers:      config.Brokers,
		topic:        config.Topic,
		partition:    partition,
		batchSize:    config.BatchSize,
		batchTimeout: config.BatchTimeout,
		writeTimeout: config.WriteTimeout,
		dialer:       config.Dialer,
		msgs:         make(chan writerMessage, config.QueueCapacity),
	}
}

func (w *writer) close() {
	close(w.msgs)
}

func (w *writer) run() {
	ticker := time.NewTicker(w.batchTimeout / 10)
	defer ticker.Stop()

	var conn *Conn
	var done bool
	var batch = make([]Message, 0, w.batchSize)
	var resch = make([](chan<- error), 0, w.batchSize)
	var lastFlushAt = time.Now()

	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	for !done {
		var mustFlush bool

		select {
		case wm, ok := <-w.msgs:
			if !ok {
				done, mustFlush = true, true
			} else {
				batch = append(batch, wm.msg)
				resch = append(resch, wm.res)
				mustFlush = len(batch) >= w.batchSize
			}

		case now := <-ticker.C:
			mustFlush = now.Sub(lastFlushAt) > w.batchTimeout
		}

		if mustFlush {
			lastFlushAt = time.Now()

			if len(batch) != 0 {
				var err error
				conn, err = w.write(conn, batch...)

				if err == nil {
					for _, res := range resch {
						res <- nil
					}
				} else {
					for i, res := range resch {
						res <- &writerError{
							msg: batch[i],
							err: err,
						}
					}
				}

				batch = batch[:0]
			}
		}
	}
}

func (w *writer) write(conn *Conn, msgs ...Message) (ret *Conn, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), w.writeTimeout)
	if conn == nil {
		conn, err = w.dial(ctx)
	}

	deadline, _ := ctx.Deadline()
	cancel()

	if conn != nil {
		conn.SetWriteDeadline(deadline)

		_, err = conn.WriteMessages(msgs...)
		conn.Close()
	}

	ret = conn
	return
}

func (w *writer) dial(ctx context.Context) (conn *Conn, err error) {
	for _, broker := range shuffledStrings(w.brokers) {
		if conn, err = w.dialer.DialLeader(ctx, "tcp", broker, w.topic, w.partition); err == nil {
			break
		}
	}
	return
}

type writerMessage struct {
	msg Message
	res chan<- error
}

type writerError struct {
	msg Message
	err error
}

func (e *writerError) Cause() error {
	return e.err
}

func (e *writerError) Error() string {
	return e.err.Error()
}

func (e *writerError) Temporary() bool {
	return isTemporary(e.err)
}

func (e *writerError) Timeout() bool {
	return isTimeout(e.err)
}

func shuffledStrings(list []string) []string {
	shuffledList := make([]string, len(list))
	copy(shuffledList, list)

	shufflerMutex.Lock()

	for i := range shuffledList {
		j := shuffler.Intn(i + 1)
		shuffledList[i], shuffledList[j] = shuffledList[j], shuffledList[i]
	}

	shufflerMutex.Unlock()
	return shuffledList
}

var (
	shufflerMutex = sync.Mutex{}
	shuffler      = rand.New(rand.NewSource(time.Now().Unix()))
)

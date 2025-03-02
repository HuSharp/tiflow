// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package kafka

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shopify/sarama"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/contextutil"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/sink/codec/common"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/sink/kafka"
	"github.com/pingcap/tiflow/pkg/util"
	"go.uber.org/zap"
)

const (
	// defaultPartitionNum specifies the default number of partitions when we create the topic.
	defaultPartitionNum = 3
)

const (
	kafkaProducerRunning = 0
	kafkaProducerClosing = 1
)

type kafkaSaramaProducer struct {
	// clientLock is used to protect concurrent access of asyncProducer and syncProducer.
	// Since we don't close these two clients (which have an input chan) from the
	// sender routine, data race or send on closed chan could happen.
	clientLock sync.RWMutex
	// This admin mainly used by `metricsMonitor` to fetch broker info.
	admin         kafka.ClusterAdminClient
	client        kafka.Client
	asyncProducer kafka.AsyncProducer
	syncProducer  kafka.SyncProducer

	// producersReleased records whether asyncProducer and syncProducer have been closed properly
	producersReleased bool

	// It is used to count the number of messages sent out and messages received when flushing data.
	mu struct {
		sync.Mutex
		inflight  int64
		flushDone chan struct{}
	}

	failpointCh chan error

	closeCh chan struct{}
	// atomic flag indicating whether the producer is closing
	closing kafkaProducerClosingFlag

	role util.Role
	id   model.ChangeFeedID
}

type kafkaProducerClosingFlag = int32

// AsyncSendMessage asynchronously sends a message to kafka.
// Notice: this method is not thread-safe.
// Do not try to call AsyncSendMessage and Flush functions in different threads,
// otherwise Flush will not work as expected. It may never finish or flush the wrong message.
// Because inflight will be modified by mistake.
func (k *kafkaSaramaProducer) AsyncSendMessage(
	ctx context.Context, topic string, partition int32, message *common.Message,
) error {
	k.clientLock.RLock()
	defer k.clientLock.RUnlock()

	// Checks whether the producer is closing.
	// The atomic flag must be checked under `clientLock.RLock()`
	if atomic.LoadInt32(&k.closing) == kafkaProducerClosing {
		return nil
	}

	failpoint.Inject("KafkaSinkAsyncSendError", func() {
		// simulate sending message to input channel successfully but flushing
		// message to Kafka meets error
		log.Info("failpoint error injected", zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
		k.failpointCh <- errors.New("kafka sink injected error")
		failpoint.Return(nil)
	})

	msg := &sarama.ProducerMessage{
		Topic:     topic,
		Key:       sarama.ByteEncoder(message.Key),
		Value:     sarama.ByteEncoder(message.Value),
		Partition: partition,
	}
	k.mu.Lock()
	k.mu.inflight++
	log.Debug("emitting inflight messages to kafka", zap.Int64("inflight", k.mu.inflight))
	k.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-k.closeCh:
		return nil
	case k.asyncProducer.Input() <- msg:
	}
	return nil
}

func (k *kafkaSaramaProducer) SyncBroadcastMessage(
	ctx context.Context, topic string, partitionsNum int32, message *common.Message,
) error {
	k.clientLock.RLock()
	defer k.clientLock.RUnlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-k.closeCh:
		return nil
	default:
		err := k.syncProducer.SendMessages(topic, partitionsNum, message.Key, message.Value)
		return cerror.WrapError(cerror.ErrKafkaSendMessage, err)
	}
}

// Flush waits for all the messages in the async producer to be sent to Kafka.
// Notice: this method is not thread-safe.
// Do not try to call AsyncSendMessage and Flush functions in different threads,
// otherwise Flush will not work as expected. It may never finish or flush the wrong message.
// Because inflight will be modified by mistake.
func (k *kafkaSaramaProducer) Flush(ctx context.Context) error {
	done := make(chan struct{}, 1)

	k.mu.Lock()
	inflight := k.mu.inflight
	immediateFlush := inflight == 0
	if !immediateFlush {
		k.mu.flushDone = done
	}
	k.mu.Unlock()

	if immediateFlush {
		return nil
	}

	log.Debug("flush waiting for inflight messages", zap.Int64("inflight", inflight))
	select {
	case <-k.closeCh:
		return cerror.ErrKafkaFlushUnfinished.GenWithStackByArgs()
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// stop closes the closeCh to signal other routines to exit
// It SHOULD NOT be called under `clientLock`.
func (k *kafkaSaramaProducer) stop() {
	if atomic.SwapInt32(&k.closing, kafkaProducerClosing) == kafkaProducerClosing {
		return
	}
	log.Info("kafka producer closing...", zap.String("namespace", k.id.Namespace),
		zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	close(k.closeCh)
}

// Close closes the sync and async clients.
func (k *kafkaSaramaProducer) Close() error {
	log.Info("stop the kafka producer", zap.String("namespace", k.id.Namespace),
		zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	k.stop()

	k.clientLock.Lock()
	defer k.clientLock.Unlock()

	if k.producersReleased {
		// We need to guard against double closing the clients,
		// which could lead to panic.
		log.Warn("kafka producer already released",
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID),
			zap.Any("role", k.role))
		return nil
	}
	k.producersReleased = true

	// `client` is mainly used by `asyncProducer` to fetch metadata and other related
	// operations. When we close the `kafkaSaramaProducer`, TiCDC no need to make sure
	// that buffered messages flushed.
	// Consider the situation that the broker does not respond, If the client is not
	// closed, `asyncProducer.Close()` would waste a mount of time to try flush all messages.
	// To prevent the scenario mentioned above, close client first.
	start := time.Now()
	if err := k.client.Close(); err != nil {
		log.Error("close sarama client with error", zap.Error(err),
			zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	} else {
		log.Info("sarama client closed", zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	}

	start = time.Now()
	err := k.asyncProducer.Close()
	if err != nil {
		log.Error("close async client with error", zap.Error(err),
			zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID),
			zap.Any("role", k.role))
	} else {
		log.Info("async client closed", zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	}
	start = time.Now()
	err = k.syncProducer.Close()
	if err != nil {
		log.Error("close sync client with error", zap.Error(err),
			zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	} else {
		log.Info("sync client closed", zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	}

	// adminClient should be closed last, since `metricsMonitor` would use it when `Cleanup`.
	start = time.Now()
	if err := k.admin.Close(); err != nil {
		log.Warn("close kafka cluster admin with error", zap.Error(err),
			zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	} else {
		log.Info("kafka cluster admin closed", zap.Duration("duration", time.Since(start)),
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
	}

	return nil
}

func (k *kafkaSaramaProducer) run(ctx context.Context) error {
	defer func() {
		log.Info("stop the kafka producer",
			zap.String("namespace", k.id.Namespace),
			zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
		k.stop()
	}()

	for {
		var ack *sarama.ProducerMessage
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-k.closeCh:
			return nil
		case err := <-k.failpointCh:
			log.Warn("receive from failpoint chan", zap.Error(err),
				zap.String("namespace", k.id.Namespace),
				zap.String("changefeed", k.id.ID), zap.Any("role", k.role))
			return err
		case ack = <-k.asyncProducer.Successes():
		case err := <-k.asyncProducer.Errors():
			// We should not wrap a nil pointer if the pointer is of a subtype of `error`
			// because Go would store the type info and the resulted `error` variable would not be nil,
			// which will cause the pkg/error library to malfunction.
			if err == nil {
				return nil
			}
			return cerror.WrapError(cerror.ErrKafkaAsyncSendMessage, err)
		}
		if ack != nil {
			k.mu.Lock()
			k.mu.inflight--
			if k.mu.inflight == 0 && k.mu.flushDone != nil {
				k.mu.flushDone <- struct{}{}
				k.mu.flushDone = nil
			}
			k.mu.Unlock()
		}
	}
}

// NewAdminClientImpl specifies the build method for the admin client.
var NewAdminClientImpl kafka.ClusterAdminClientCreator = kafka.NewSaramaAdminClient

// NewClientImpl specifies the build method for the  client.
var NewClientImpl kafka.ClientCreator = kafka.NewSaramaClient

// NewKafkaSaramaProducer creates a kafka sarama producer
func NewKafkaSaramaProducer(
	ctx context.Context,
	client kafka.Client,
	admin kafka.ClusterAdminClient,
	options *kafka.Options,
	errCh chan error,
	changefeedID model.ChangeFeedID,
) (*kafkaSaramaProducer, error) {
	role := contextutil.RoleFromCtx(ctx)
	log.Info("Starting kafka sarama producer ...", zap.Any("options", options),
		zap.String("namespace", changefeedID.Namespace),
		zap.String("changefeed", changefeedID.ID), zap.Any("role", role))

	asyncProducer, err := client.AsyncProducer()
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}

	syncProducer, err := client.SyncProducer()
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}

	runSaramaMetricsMonitor(ctx, client.MetricRegistry(), changefeedID, role, admin)

	k := &kafkaSaramaProducer{
		admin:         admin,
		client:        client,
		asyncProducer: asyncProducer,
		syncProducer:  syncProducer,
		closeCh:       make(chan struct{}),
		failpointCh:   make(chan error, 1),
		closing:       kafkaProducerRunning,

		id:   changefeedID,
		role: role,
	}
	go func() {
		if err := k.run(ctx); err != nil && errors.Cause(err) != context.Canceled {
			select {
			case <-ctx.Done():
				return
			case errCh <- err:
			default:
				log.Error("error channel is full", zap.Error(err),
					zap.String("namespace", k.id.Namespace),
					zap.String("changefeed", k.id.ID), zap.Any("role", role))
			}
		}
	}()
	return k, nil
}

// AdjustOptions adjust the `Options` and `sarama.Config` by condition.
func AdjustOptions(
	admin kafka.ClusterAdminClient,
	options *kafka.Options,
	topic string,
) error {
	topics, err := admin.GetAllTopicsMeta()
	if err != nil {
		return errors.Trace(err)
	}

	err = validateMinInsyncReplicas(admin, topics, topic, int(options.ReplicationFactor))
	if err != nil {
		return errors.Trace(err)
	}

	info, exists := topics[topic]
	// once we have found the topic, no matter `auto-create-topic`, make sure user input parameters are valid.
	if exists {
		// make sure that producer's `MaxMessageBytes` smaller than topic's `max.message.bytes`
		topicMaxMessageBytesStr, err := getTopicConfig(admin, info, kafka.TopicMaxMessageBytesConfigName,
			kafka.BrokerMessageMaxBytesConfigName)
		if err != nil {
			return errors.Trace(err)
		}
		topicMaxMessageBytes, err := strconv.Atoi(topicMaxMessageBytesStr)
		if err != nil {
			return errors.Trace(err)
		}

		if topicMaxMessageBytes < options.MaxMessageBytes {
			log.Warn("topic's `max.message.bytes` less than the `max-message-bytes`,"+
				"use topic's `max.message.bytes` to initialize the Kafka producer",
				zap.Int("max.message.bytes", topicMaxMessageBytes),
				zap.Int("max-message-bytes", options.MaxMessageBytes))
			options.MaxMessageBytes = topicMaxMessageBytes
		}

		// no need to create the topic, but we would have to log user if they found enter wrong topic name later
		if options.AutoCreate {
			log.Warn("topic already exist, TiCDC will not create the topic",
				zap.String("topic", topic), zap.Any("detail", info))
		}

		if err := options.SetPartitionNum(info.NumPartitions); err != nil {
			return errors.Trace(err)
		}

		return nil
	}

	brokerMessageMaxBytesStr, err := admin.GetBrokerConfig(kafka.BrokerMessageMaxBytesConfigName)
	if err != nil {
		log.Warn("TiCDC cannot find `message.max.bytes` from broker's configuration")
		return errors.Trace(err)
	}
	brokerMessageMaxBytes, err := strconv.Atoi(brokerMessageMaxBytesStr)
	if err != nil {
		return errors.Trace(err)
	}

	// when create the topic, `max.message.bytes` is decided by the broker,
	// it would use broker's `message.max.bytes` to set topic's `max.message.bytes`.
	// TiCDC need to make sure that the producer's `MaxMessageBytes` won't larger than
	// broker's `message.max.bytes`.
	if brokerMessageMaxBytes < options.MaxMessageBytes {
		log.Warn("broker's `message.max.bytes` less than the `max-message-bytes`,"+
			"use broker's `message.max.bytes` to initialize the Kafka producer",
			zap.Int("message.max.bytes", brokerMessageMaxBytes),
			zap.Int("max-message-bytes", options.MaxMessageBytes))
		options.MaxMessageBytes = brokerMessageMaxBytes
	}

	// topic not exists yet, and user does not specify the `partition-num` in the sink uri.
	if options.PartitionNum == 0 {
		options.PartitionNum = defaultPartitionNum
		log.Warn("partition-num is not set, use the default partition count",
			zap.String("topic", topic), zap.Int32("partitions", options.PartitionNum))
	}
	return nil
}

func validateMinInsyncReplicas(
	admin kafka.ClusterAdminClient,
	topics map[string]kafka.TopicDetail, topic string, replicationFactor int,
) error {
	minInsyncReplicasConfigGetter := func() (string, bool, error) {
		info, exists := topics[topic]
		if exists {
			minInsyncReplicasStr, err := getTopicConfig(admin, info,
				kafka.MinInsyncReplicasConfigName,
				kafka.MinInsyncReplicasConfigName)
			if err != nil {
				return "", true, err
			}
			return minInsyncReplicasStr, true, nil
		}

		minInsyncReplicasStr, err := admin.GetBrokerConfig(kafka.MinInsyncReplicasConfigName)
		if err != nil {
			return "", false, err
		}

		return minInsyncReplicasStr, false, nil
	}

	minInsyncReplicasStr, exists, err := minInsyncReplicasConfigGetter()
	if err != nil {
		// 'min.insync.replica' is invisible to us in Confluent Cloud Kafka.
		if cerror.ErrKafkaBrokerConfigNotFound.Equal(err) {
			return nil
		}
		return err
	}
	minInsyncReplicas, err := strconv.Atoi(minInsyncReplicasStr)
	if err != nil {
		return err
	}

	configFrom := "topic"
	if !exists {
		configFrom = "broker"
	}

	if replicationFactor < minInsyncReplicas {
		msg := fmt.Sprintf("`replication-factor` cannot be smaller than the `%s` of %s",
			kafka.MinInsyncReplicasConfigName, configFrom)
		log.Error(msg, zap.Int("replication-factor", replicationFactor),
			zap.Int("min.insync.replicas", minInsyncReplicas))
		return cerror.ErrKafkaInvalidConfig.GenWithStack(
			"TiCDC Kafka producer's `request.required.acks` defaults to -1, "+
				"TiCDC cannot deliver messages when the `replication-factor` "+
				"is smaller than the `min.insync.replicas` of %s", configFrom,
		)
	}

	return nil
}

// getTopicConfig gets topic config by name.
// If the topic does not have this configuration, we will try to get it from the broker's configuration.
// NOTICE: The configuration names of topic and broker may be different for the same configuration.
func getTopicConfig(
	admin kafka.ClusterAdminClient,
	detail kafka.TopicDetail,
	topicConfigName string,
	brokerConfigName string,
) (string, error) {
	if a, ok := detail.ConfigEntries[topicConfigName]; ok {
		return a, nil
	}

	return admin.GetBrokerConfig(brokerConfigName)
}

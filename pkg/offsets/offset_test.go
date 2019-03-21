package offsets

import (
	"os"
	"strings"
	"testing"
	"time"

	"encoding/binary"
	"hash/crc32"
	"math/rand"

	"github.com/Shopify/sarama"
	. "github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb-binlog/pkg/assemble"
	"github.com/pingcap/tidb-binlog/pump"
	"github.com/pingcap/tipb/go-binlog"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
)

var (
	_        = Suite(&testOffsetSuite{})
	crcTable = crc32.MakeTable(crc32.Castagnoli)
)

func TestClient(t *testing.T) {
	TestingT(t)
}

type testOffsetSuite struct {
	producer sarama.SyncProducer
}

func (to *testOffsetSuite) TestOffset(c *C) {
	kafkaAddr := "127.0.0.1"
	if os.Getenv("HOSTIP") != "" {
		kafkaAddr = os.Getenv("HOSTIP")
	}
	kafkaAddr = kafkaAddr + ":9092"
	topic := "offset_test"

	config := sarama.NewConfig()
	config.Version = sarama.V1_0_0_0
	config.Producer.Partitioner = sarama.NewManualPartitioner
	config.Producer.Return.Successes = true

	_, err := sarama.NewClient(strings.Split(kafkaAddr, ","), config)
	if err != nil && strings.Contains(err.Error(), "client has run out of available brokers") {
		c.Skip("no kafka available")
	}

	// clear previous tests produced
	to.deleteTopic(kafkaAddr, config, topic, c)
	// tear down or clear up
	defer to.deleteTopic(kafkaAddr, config, topic, c)

	sk, err := NewKafkaSeeker([]string{kafkaAddr}, config, PositionOperator{})
	c.Assert(err, IsNil)
	defer sk.Close()

	to.producer, err = sarama.NewSyncProducer([]string{kafkaAddr}, config)
	c.Assert(err, IsNil)
	defer to.producer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var testDatas = []string{"b", "d", "e"}
	var testPoss = map[string]int64{
		"b": 0,
		"d": 0,
		"e": 0,
	}
	for _, m := range testDatas {
		testPoss[m], err = to.procudeMessage([]byte(m), topic)
		c.Assert(err, IsNil)
	}

	var testCases = map[string]int64{
		"a": testPoss["b"],
		"c": testPoss["b"],
		"b": testPoss["b"],
		"h": testPoss["e"],
	}
	for m, res := range testCases {
		offsetFounds, err := sk.Do(ctx, topic, m, 0, 0, []int32{0})
		c.Assert(err, IsNil)
		c.Assert(offsetFounds, HasLen, 1)
		c.Assert(offsetFounds[0], Equals, res)
	}

	sli := pump.NewKafkaSlicer(topic, 0)
	pump.GlobalConfig.EnableBinlogSlice = true
	pump.GlobalConfig.SlicesSize = 4

	// offset seek for slice messages
	message := []byte("aaaaaaaaaaaaaaaaaaaa")
	entity := to.genBinlogEntity(message, 1, 2)
	messages, err := sli.Generate(entity)
	c.Assert(err, IsNil)
	offset, err := to.produceMessageSlices(messages)
	c.Assert(err, IsNil)
	offsetFounds, err := sk.Do(ctx, topic, string(message), 0, 0, []int32{0})
	c.Assert(err, IsNil)
	c.Assert(offsetFounds, HasLen, 1)
	c.Assert(offsetFounds[0], Equals, offset)

	// offset seek for slice messages, out-of-order
	message = []byte("bbbbbbbbbbbbbbbbbbbb")
	entity = to.genBinlogEntity(message, 2, 3)
	messages, err = sli.Generate(entity)
	c.Assert(err, IsNil)
	rand.Shuffle(len(messages), func(i, j int) {
		messages[i], messages[j] = messages[j], messages[i]
	})
	offset, err = to.produceMessageSlices(messages)
	c.Assert(err, IsNil)
	offsetFounds, err = sk.Do(ctx, topic, string(message), 0, 0, []int32{0})
	c.Assert(err, IsNil)
	c.Assert(offsetFounds, HasLen, 1)
	c.Assert(offsetFounds[0], Equals, offset)

	// offset seek for slice messages,  out-of-order and duplicated
	message = []byte("cccccccccccccccccccc")
	entity = to.genBinlogEntity(message, 3, 4)
	messages, err = sli.Generate(entity)
	c.Assert(err, IsNil)
	rand.Shuffle(len(messages), func(i, j int) {
		messages[i], messages[j] = messages[j], messages[i]
	})
	dupSlices := make([]*sarama.ProducerMessage, len(messages)+2)
	dupSlices[0] = messages[0]
	dupSlices[len(dupSlices)-1] = messages[len(messages)-1]
	for i, slice := range messages {
		dupSlices[i+1] = slice
	}
	offset, err = to.produceMessageSlices(dupSlices)
	c.Assert(err, IsNil)
	offsetFounds, err = sk.Do(ctx, topic, string(message), 0, 0, []int32{0})
	c.Assert(err, IsNil)
	c.Assert(offsetFounds, HasLen, 1)
	c.Assert(offsetFounds[0], Equals, offset)

	// complete binlog slices follow incomplete slices
	message = []byte("dddddddddddddddddddd")
	entity = to.genBinlogEntity(message, 4, 5)
	messages, err = sli.Generate(entity)
	c.Assert(err, IsNil)
	messages = messages[1:] // drop a slice
	_, err = to.produceMessageSlices(messages)
	c.Assert(err, IsNil)

	message = []byte("eeeeeeeeeeeeeeeeeeee")
	entity = to.genBinlogEntity(message, 1, 2)
	messages, err = sli.Generate(entity)
	c.Assert(err, IsNil)
	offset, err = to.produceMessageSlices(messages)
	c.Assert(err, IsNil)
	offsetFounds, err = sk.Do(ctx, topic, string(message), 0, 0, []int32{0})
	c.Assert(err, IsNil)
	c.Assert(offsetFounds, HasLen, 1)
	c.Assert(offsetFounds[0], Equals, offset)
}

func (to *testOffsetSuite) deleteTopic(kafkaAddr string, config *sarama.Config, topic string, c *C) {
	// delete topic to clear produced messages
	broker := sarama.NewBroker(kafkaAddr)
	err := broker.Open(config)
	c.Assert(err, IsNil)
	_, err = broker.Connected()
	c.Assert(err, IsNil)
	defer broker.Close()
	broker.DeleteTopics(&sarama.DeleteTopicsRequest{Topics: []string{topic}, Timeout: 30 * time.Second})
}

func (to *testOffsetSuite) genBinlogEntity(message []byte, suffix uint64, offset int64) *binlog.Entity {
	crc := crc32.Checksum(message, crcTable)
	checksum := make([]byte, 4)
	binary.LittleEndian.PutUint32(checksum, crc)
	return &binlog.Entity{
		Pos: binlog.Pos{
			Suffix: suffix,
			Offset: offset,
		},
		Payload:  message,
		Checksum: checksum,
	}
}

func (to *testOffsetSuite) procudeMessage(message []byte, topic string) (int64, error) {
	var (
		offset int64
		err    error
	)
	for i := 0; i < 5; i++ {
		msg := &sarama.ProducerMessage{
			Topic:     topic,
			Partition: int32(0),
			Key:       sarama.StringEncoder("key"),
			Value:     sarama.ByteEncoder(message),
		}
		_, offset, err = to.producer.SendMessage(msg)
		if err == nil {
			return offset, errors.Trace(err)
		}

		time.Sleep(time.Second)
	}

	return offset, err
}

func (to *testOffsetSuite) produceMessageSlices(slices []*sarama.ProducerMessage) (int64, error) {
	var (
		offset int64
		err    error
		j      int
		slice  *sarama.ProducerMessage
	)
	for i := 0; i < 5; i++ {
		for j, slice = range slices {
			_, offsetSlice, err := to.producer.SendMessage(slice)
			if err != nil {
				if j == 0 {
					time.Sleep(time.Second)
					break // the first slice send fail, outer for loop try again
				}
				return offset, errors.Trace(err)
			}
			if j == 0 {
				offset = offsetSlice // saves for return
			}
		}
		if j == len(slices)-1 {
			break // all slices sent
		}
	}
	return offset, err
}

type PositionOperator struct{}

// Compare implements Operator.Compare interface
func (p PositionOperator) Compare(exceptedPos interface{}, currentPos interface{}) (int, error) {
	b, ok := currentPos.(string)
	if !ok {
		return 0, errors.Errorf("fail to convert %v type to string", currentPos)
	}

	a, ok := exceptedPos.(string)
	if !ok {
		return 0, errors.Errorf("fail to convert %v type to string", exceptedPos)
	}

	if a > b || len(a) > len(b) { // maybe a slice message
		return 1, nil
	}
	if a == b {
		return 0, nil
	}

	return -1, nil
}

// Decode implements Operator.Decode interface
func (p PositionOperator) Decode(ctx context.Context, messages <-chan *sarama.ConsumerMessage) (interface{}, int64, error) {
	errCounter := prometheus.NewCounter(prometheus.CounterOpts{})
	asm := assemble.NewAssembler(errCounter)
	defer asm.Close()

	var binlog2 *assemble.AssembledBinlog
	for {
		select {
		case <-ctx.Done():
			return nil, 0, errors.New("offset seeker was canceled")
		case msg := <-messages:
			asm.Append(msg)
		case binlog2 = <-asm.Messages():
		}
		if binlog2 != nil {
			break
		}
	}
	return string(binlog2.Entity.Payload), binlog2.Entity.Pos.Offset, nil
}
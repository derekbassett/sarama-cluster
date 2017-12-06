package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("PartitionSlice", func() {

	It("should sort correctly", func() {
		p1 := Partition{Addr: "host1:9093", ID: 1}
		p2 := Partition{Addr: "host1:9092", ID: 2}
		p3 := Partition{Addr: "host2:9092", ID: 3}
		p4 := Partition{Addr: "host3:9091", ID: 4}
		p5 := Partition{Addr: "host2:9093", ID: 5}
		p6 := Partition{Addr: "host1:9092", ID: 6}

		slice := PartitionSlice{p1, p2, p3, p4, p5, p6}
		sort.Sort(slice)
		Expect(slice).To(BeEquivalentTo(PartitionSlice{p2, p6, p1, p3, p5, p4}))
	})

})

// --------------------------------------------------------------------

const (
	tTopicA = "sarama-cluster-topic-a"
	tTopicB = "sarama-cluster-topic-b"
	tTopicX = "sarama-cluster-topic-x"
	tGroup  = "sarama-cluster-group"
	tGroupX = "sarama-cluster-group-x"
	tDir    = "/tmp/sarama-cluster-test"
)

var (
	tKafkaDir   = "kafka_2.11-0.8.2.1"
	tKafkaAddrs = []string{"127.0.0.1:29092"}
	tZKAddrs    = []string{"127.0.0.1:22181"}
	tN          = 100000
)

func init() {
	if dir := os.Getenv("KAFKA_DIR"); dir != "" {
		tKafkaDir = dir
	}
	if testing.Short() {
		tN = 10000
	}
}

// --------------------------------------------------------------------

var _ = BeforeSuite(func() {
	run := testDir(tKafkaDir, "bin", "kafka-run-class.sh")
	cli := testDir(tKafkaDir, "bin", "kafka-topics.sh")
	scenario.zk = exec.Command(run, "-name", "zookeeper", "org.apache.zookeeper.server.ZooKeeperServerMain", testDir("zookeeper.properties"))
	// scenario.zk.Stderr = os.Stderr
	// scenario.zk.Stdout = os.Stdout

	scenario.kafka = exec.Command(run, "-name", "kafkaServer", "kafka.Kafka", testDir("server.properties"))
	scenario.kafka.Env = []string{"KAFKA_HEAP_OPTS=-Xmx1G -Xms1G"}
	// scenario.kafka.Stderr = os.Stderr
	// scenario.kafka.Stdout = os.Stdout

	// Create Dir
	Expect(os.MkdirAll(tDir, 0775)).NotTo(HaveOccurred())

	// Start ZK & Kafka
	Expect(scenario.zk.Start()).NotTo(HaveOccurred())
	Expect(scenario.kafka.Start()).NotTo(HaveOccurred())

	// Wait for client
	var client sarama.Client
	Eventually(func() error {
		var err error
		client, err = sarama.NewClient(tKafkaAddrs, nil)
		return err
	}, "10s", "1s").ShouldNot(HaveOccurred())
	defer client.Close()

	// Ensure we can retrieve partition info
	Eventually(func() error {
		_, err := client.Partitions(tTopicA)
		return err
	}, "10s", "1s").ShouldNot(HaveOccurred())

	// Create a special truncated topic B with 6 topics
	cmd := exec.Command(cli, "--zookeeper", "localhost:22181", "--create", "--topic", tTopicB, "--partitions", "6", "--replication-factor", "1")
	Expect(cmd.Run()).NotTo(HaveOccurred())

	// Create a special truncated topic X with a small retention config
	cmd = exec.Command(cli, "--zookeeper", "localhost:22181", "--create", "--topic", tTopicX, "--partitions", "1", "--replication-factor", "1", "--config", "segment.bytes=1024", "--config", "retention.bytes=4096")
	Expect(cmd.Run()).NotTo(HaveOccurred())

	// Seed messages to topic A
	pA, err := sarama.NewAsyncProducerFromClient(client)
	Expect(err).NotTo(HaveOccurred())
	for i := 0; i < tN; i++ {
		kv := sarama.StringEncoder(fmt.Sprintf("TOPIC-A-%08d", i))
		pA.Input() <- &sarama.ProducerMessage{Topic: tTopicA, Key: kv, Value: kv}
	}
	Expect(pA.Close()).NotTo(HaveOccurred())

	// Seed messages to topic B
	pB, err := sarama.NewAsyncProducerFromClient(client)
	Expect(err).NotTo(HaveOccurred())
	for i := 0; i < tN; i++ {
		kv := sarama.StringEncoder(fmt.Sprintf("TOPIC-B-%08d", i))
		pB.Input() <- &sarama.ProducerMessage{Topic: tTopicB, Key: kv, Value: kv}
	}
	Expect(pB.Close()).NotTo(HaveOccurred())

	// Seed messages to (truncated) topic X
	pX, err := sarama.NewSyncProducerFromClient(client)
	Expect(err).NotTo(HaveOccurred())
	for i := 0; i < 100; i++ {
		kv := sarama.StringEncoder(fmt.Sprintf("TOPIC-X-%08d", i))
		_, _, err := pX.SendMessage(&sarama.ProducerMessage{Topic: tTopicX, Key: kv, Value: kv})
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(pX.Close()).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if scenario.kafka != nil {
		scenario.kafka.Process.Kill()
	}
	if scenario.zk != nil {
		scenario.zk.Process.Kill()
	}
	Expect(os.RemoveAll(tDir)).NotTo(HaveOccurred())
})

func TestSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	AfterEach(func() {
		zk, err := NewZK(tZKAddrs, time.Second)
		Expect(err).NotTo(HaveOccurred())

		zk.DeleteAll("/consumers/" + tGroup)
		zk.Close()
	})
	RunSpecs(t, "sarama/cluster")
}

// --------------------------------------------------------------------

var scenario struct{ kafka, zk *exec.Cmd }

func newConsumer(topics []string, conf *Config) (*Consumer, error) {
	if topics == nil {
		topics = []string{tTopicA, tTopicB}
	}
	return NewConsumer(tKafkaAddrs, tZKAddrs, tGroup, topics, conf)
}

func testDir(tokens ...string) string {
	tokens = append([]string{"_test"}, tokens...)
	return filepath.Join(tokens...)
}

// --------------------------------------------------------------------

type mockNotifier struct {
	lock     sync.Mutex
	messages []string
}

func (n *mockNotifier) RebalanceStart(c *Consumer) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.messages = append(n.messages, "REBALANCE START")
}
func (n *mockNotifier) RebalanceOK(c *Consumer) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.messages = append(n.messages, "REBALANCE OK")
}
func (n *mockNotifier) RebalanceError(c *Consumer, err error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.messages = append(n.messages, "REBALANCE ERROR")
}
func (n *mockNotifier) CommitError(c *Consumer, err error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.messages = append(n.messages, "COMMIT ERROR")
}
func (n *mockNotifier) Messages() []string {
	n.lock.Lock()
	defer n.lock.Unlock()
	msgs := make([]string, len(n.messages))
	copy(msgs, n.messages)
	return msgs
}

package pkg
import (
	"time"

	"k8s.io/heapster/metrics/core"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"strconv"
)

const (
	DefaultScrapeOffset   = 5 * time.Second
	DefaultMaxParallelism = 3
)

var (
	// The Time spent in a processor in microseconds.
	processorDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "heapster",
			Subsystem: "processor",
			Name:      "duration_microseconds",
			Help:      "The Time spent in a processor in microseconds.",
		},
		[]string{"processor"},
	)
)

func init() {
	prometheus.MustRegister(processorDuration)
}

type Manager interface {
	Start()
	Stop()
}

type realManager struct {
	source                 core.MetricsSource
	processors             []core.DataProcessor
	resolution             time.Duration
	scrapeOffset           time.Duration
	stopChan               chan struct{}
	housekeepSemaphoreChan chan struct{}
	housekeepTimeout       time.Duration
}

func NewManager(source core.MetricsSource, processors []core.DataProcessor, resolution time.Duration,
	scrapeOffset time.Duration, maxParallelism int) (Manager, error) {
	manager := realManager{
		source:                 source,
		processors:             processors,
		resolution:             resolution,
		scrapeOffset:           scrapeOffset,
		stopChan:               make(chan struct{}),
		housekeepSemaphoreChan: make(chan struct{}, maxParallelism),
		housekeepTimeout:       resolution / 2,
	}

	for i := 0; i < maxParallelism; i++ {
		manager.housekeepSemaphoreChan <- struct{}{}
	}

	return &manager, nil
}

func (rm *realManager) Start() {
	go rm.Housekeep()
}

func (rm *realManager) Stop() {
	rm.stopChan <- struct{}{}
}

func (rm *realManager) Housekeep() {
	for {
		// Always try to get the newest metrics
		now := time.Now()
		start := now.Truncate(rm.resolution)
		end := start.Add(rm.resolution)
		timeToNextSync := end.Add(rm.scrapeOffset).Sub(now)

		select {
		case <-time.After(timeToNextSync):
			rm.housekeep(start, end)
		case <-rm.stopChan:
			return
		}
	}
}

func (rm *realManager) housekeep(start, end time.Time) {
	if !start.Before(end) {
		glog.Warningf("Wrong time provided to housekeep start:%s end: %s", start, end)
		return
	}

	select {
	case <-rm.housekeepSemaphoreChan:
		// ok, good to go

	case <-time.After(rm.housekeepTimeout):
		glog.Warningf("Spent too long waiting for housekeeping to start")
		return
	}

	go func(rm *realManager) {
		// should always give back the semaphore
		defer func() { rm.housekeepSemaphoreChan <- struct{}{} }()
		//从这里获取load？
		data := rm.source.ScrapeMetrics(start, end)

		for _, p := range rm.processors {
			newData, err := process(p, data)
			if err == nil {
				data = newData
			} else {
				glog.Errorf("Error in processor: %v", err)
				return
			}
		}

		glog.Info("这里是数据------------------------------")
		i:= 0
		for key,value := range data.MetricSets{
			glog.Info("data"+strconv.Itoa(i)+": -----------")
			i+=1
			glog.Info("key: "+key)
			glog.Info("createTime: ")
			glog.Info(value.CreateTime)
			glog.Info("metric value: ")
			j:=0
			for key2,value2 := range value.MetricValues{
				glog.Info("subkey"+strconv.Itoa(j)+": "+key2+"-----------")
				j+=1
				glog.Info(value2.IntValue)
			}
			j=0
			glog.Info("labeled value: ")
			for _,value2 := range value.LabeledMetrics{
				glog.Info("subkey-l"+strconv.Itoa(j)+": -----------")
				j+=1
				glog.Info(value2.Name)
				glog.Info(value2.IntValue)
				glog.Info(value2.MetricType)
			}
			j=0
			glog.Info("labels: ")
			for key2,value2 := range value.Labels{
				glog.Info("subkey-l"+strconv.Itoa(j)+": "+key2+" -----------")
				j+=1
				glog.Info(value2)
			}
		}
		//glog.Info(data)
		//glog.Info(data.MetricSets["sdf"].MetricValues["sdfsd"].IntValue)
		// Export data to sinks
		//rm.sink.ExportData(data)

	}(rm)
}

func process(p core.DataProcessor, data *core.DataBatch) (*core.DataBatch, error) {
	startTime := time.Now()
	defer processorDuration.
	WithLabelValues(p.Name()).
		Observe(float64(time.Since(startTime)) / float64(time.Microsecond))

	return p.Process(data)
}


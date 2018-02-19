package pkg

import (
	"time"

	"k8s.io/heapster/metrics/core"

	"container/list"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tealeg/xlsx"
)

const (
	DefaultScrapeOffset   = 5 * time.Second
	DefaultMaxParallelism = 3
	DataSumSize           = 6
	LoadDataFilePath      = "/home/load/load.txt"
	LoadDataExeclPath     = "/home/load/load"
	LoadDataExeclSuffix   = ".xlsx"
	LoadClusterSheet      = "cluster"
	LoadNodeSheet         = "node"
	LoadNamespaceSheet    = "namespace"
	LoadPodSheet          = "pod"
	LoadContainerSheet    = "container"
	LoadDataFlushMax      = 6
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
	DataSum() *map[string]*MetricSet2
}

type realManager struct {
	source                 core.MetricsSource
	processors             []core.DataProcessor
	resolution             time.Duration
	scrapeOffset           time.Duration
	stopChan               chan struct{}
	housekeepSemaphoreChan chan struct{}
	housekeepTimeout       time.Duration
	data                   *dataSum
	loadResyncTime         int
	loadFile               *LoadFile
}

type LoadFile struct {
	FileName     string
	StringOrder1 []string
	StringOrder2 []string
}

type MetricSet2 struct {
	CreateTime     time.Time
	ScrapeTime     time.Time
	MetricValues   map[string]*core.MetricValue
	Labels         map[string]string
	LabeledMetrics map[string]*core.LabeledMetric
}

type dataSum struct {
	historicalData map[string]*list.List
	sum            map[string]*MetricSet2
}

func NewManager(source core.MetricsSource, processors []core.DataProcessor, resolution time.Duration,
	scrapeOffset time.Duration, maxParallelism int) (Manager, error) {
	datasum := dataSum{
		historicalData: make(map[string]*list.List), sum: make(map[string]*MetricSet2),
	}

	var stringOrder1 []string
	var stringOrder2 []string
	for _, value := range core.StandardMetrics {
		stringOrder1 = append(stringOrder1, value.Name)
	}
	for _, value := range core.LabeledMetrics {
		stringOrder2 = append(stringOrder2, value.Name)
	}
	manager := realManager{
		source:                 source,
		processors:             processors,
		resolution:             resolution,
		scrapeOffset:           scrapeOffset,
		stopChan:               make(chan struct{}),
		housekeepSemaphoreChan: make(chan struct{}, maxParallelism),
		housekeepTimeout:       resolution / 2,
		data:                   &datasum,
		loadResyncTime:         0,
		loadFile:               &LoadFile{FileName: LoadDataExeclPath + time.Now().String()[0:19] + LoadDataExeclSuffix, StringOrder1: stringOrder1, StringOrder2: stringOrder2},
	}

	for i := 0; i < maxParallelism; i++ {
		manager.housekeepSemaphoreChan <- struct{}{}
	}

	//这里需要新建一个xlsx文件
	var file *xlsx.File
	var sheet *xlsx.Sheet
	var row *xlsx.Row
	var err error

	file = xlsx.NewFile()
	for _, value := range []string{LoadClusterSheet, LoadNodeSheet, LoadNamespaceSheet, LoadPodSheet, LoadContainerSheet} {
		sheet, err = file.AddSheet(value)
		if err != nil {
			glog.Info(err.Error())
		}
		row = sheet.AddRow()
		row.AddCell().Value = "name"
		for i := 0; i < len(manager.loadFile.StringOrder1); i++ {
			row.AddCell().Value = manager.loadFile.StringOrder1[i]
		}
		for i := 0; i < len(manager.loadFile.StringOrder2); i++ {
			row.AddCell().Value = manager.loadFile.StringOrder2[i]
		}
	}
	err = file.Save(manager.loadFile.FileName)
	if err != nil {
		glog.Info(err.Error())
	}
	return &manager, nil
}

func (rm *realManager) Start() {
	go rm.Housekeep()
}

func (rm *realManager) Stop() {
	rm.stopChan <- struct{}{}
}

func (rm *realManager) DataSum() *map[string]*MetricSet2 {
	return &rm.data.sum
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

//根据原本的core.metricSet类，返回自定义的方便统计数据的MetricSet类
func copyMetricSet(set *core.MetricSet) *MetricSet2 {
	metricValue := make(map[string]*core.MetricValue)
	label := make(map[string]string)
	labeledMetric := make(map[string]*core.LabeledMetric)
	for _, value := range set.LabeledMetrics {
		labeledMetric[value.Name] = &core.LabeledMetric{Name: value.Name, Labels: value.Labels, MetricValue: value.MetricValue}
	}
	for key, value := range set.Labels {
		label[key] = value
	}
	for key, value := range set.MetricValues {
		metricValue[key] = &core.MetricValue{IntValue: value.IntValue, FloatValue: value.FloatValue, MetricType: value.MetricType, ValueType: value.ValueType}
	}
	return &MetricSet2{CreateTime: set.CreateTime, ScrapeTime: set.ScrapeTime, Labels: label, MetricValues: metricValue, LabeledMetrics: labeledMetric}
}
func (rm *realManager) writeLoadFile() {
	//glog.Info("开始写数据-------------------")
	file, err1 := os.OpenFile(LoadDataFilePath, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
	if err1 != nil {
		glog.Info(err1)
	}
	defer file.Close()

	_, err1 = io.WriteString(file, "数据时间"+time.Now().String()+"----------------")
	if err1 != nil {
		glog.Info(err1)
	}
	for k1, _ := range rm.data.historicalData {
		_, err1 := io.WriteString(file, "负载源: "+k1+"------------------\n")
		if err1 != nil {
			glog.Info(err1)
		}
		for key, value := range rm.data.sum[k1].MetricValues {
			_, err1 := io.WriteString(file, key+"  :"+strconv.FormatInt(value.IntValue, 10)+"\n")
			if err1 != nil {
				glog.Info(err1)
			}
		}
		for _, value := range rm.data.sum[k1].LabeledMetrics {
			_, err1 := io.WriteString(file, value.Name+strconv.FormatInt(value.IntValue, 10)+"\n")
			if err1 != nil {
				glog.Info(err1)
			}
		}
	}
	_, err1 = io.WriteString(file, "-------------------------------------")
	if err1 != nil {
		glog.Info(err1)
	}

}
func (rm *realManager) consumeData2Xlxs(batch *core.DataBatch) {
	var file *xlsx.File
	var err error

	file, err = xlsx.OpenFile(rm.loadFile.FileName)
	if err != nil {
		glog.Info(err.Error())
	}

	for metricSourceName, metric := range batch.MetricSets {
		value, ok := rm.data.historicalData[metricSourceName]
		if !ok {
			value = list.New()
			value.PushBack(metric)
			rm.data.historicalData[metricSourceName] = value
		} else {
			if value.Len() < DataSumSize {
				value.PushBack(metric)
			} else {
				s1 := value.Front()
				value.Remove(s1)
				value.PushBack(metric)
			}
		}
		var sheet *xlsx.Sheet
		if strings.Contains(metricSourceName, "cluster") {
			sheet = file.Sheet[LoadClusterSheet]
		} else if strings.Contains(metricSourceName, "node") {
			sheet = file.Sheet[LoadNodeSheet]
		} else if strings.Contains(metricSourceName, "container") {
			sheet = file.Sheet[LoadContainerSheet]
		} else if strings.Contains(metricSourceName, "pod") {
			sheet = file.Sheet[LoadPodSheet]
		} else {
			sheet = file.Sheet[LoadNamespaceSheet]
		}
		row := sheet.AddRow()
		row.AddCell().Value = metricSourceName
		for i := 0; i < len(rm.loadFile.StringOrder1); i++ {
			value, ok := metric.MetricValues[rm.loadFile.StringOrder1[i]]
			if !ok {
				row.AddCell().Value = "null"
			} else {
				row.AddCell().Value = strconv.FormatInt(value.IntValue, 10)
			}

		}
		for i := 0; i < len(rm.loadFile.StringOrder2); i++ {
			var j int
			for j = 0; j < len(metric.LabeledMetrics); j++ {
				if metric.LabeledMetrics[j].Name == rm.loadFile.StringOrder2[i] {
					row.AddCell().Value = strconv.FormatInt(metric.LabeledMetrics[j].IntValue, 10)
					break
				}
			}
			if j == len(metric.LabeledMetrics) {
				row.AddCell().Value = "null"
			}
		}
	}
	err = file.Save(rm.loadFile.FileName)
	if err != nil {
		glog.Info(err.Error())
	}

}

//将获得的新数据，采进统计数据和历史数据
func (rm *realManager) consumeData(batch *core.DataBatch) {
	rm.loadResyncTime = (rm.loadResyncTime + 1) % 6

	for metricSourceName, metric := range batch.MetricSets {
		value, ok := rm.data.historicalData[metricSourceName]
		if !ok {
			value = list.New()
			rm.data.sum[metricSourceName] = copyMetricSet(metric)
			value.PushBack(metric)
			rm.data.historicalData[metricSourceName] = value
		} else {
			if value.Len() < DataSumSize {
				for _, v1 := range metric.LabeledMetrics {
					_, ok2 := rm.data.sum[metricSourceName].LabeledMetrics[v1.Name]
					if !ok2 {
						rm.data.sum[metricSourceName].LabeledMetrics[v1.Name] = &core.LabeledMetric{Name: v1.Name, Labels: v1.Labels, MetricValue: v1.MetricValue}
					} else {
						rm.data.sum[metricSourceName].LabeledMetrics[v1.Name].IntValue = ((rm.data.sum[metricSourceName].LabeledMetrics[v1.Name].IntValue*int64(value.Len()) + v1.IntValue) / int64(value.Len()+1))
					}

				}
				for k1, v1 := range metric.MetricValues {
					_, ok2 := rm.data.sum[metricSourceName].MetricValues[k1]
					if !ok2 {
						rm.data.sum[metricSourceName].MetricValues[k1] = &core.MetricValue{IntValue: v1.IntValue, FloatValue: v1.FloatValue, MetricType: v1.MetricType, ValueType: v1.ValueType}
					} else {
						rm.data.sum[metricSourceName].MetricValues[k1].IntValue = (rm.data.sum[metricSourceName].MetricValues[k1].IntValue*int64(value.Len()) + v1.IntValue) / int64(value.Len()+1)
					}
				}
				value.PushBack(metric)
			} else {
				s1 := value.Front()
				value.Remove(s1)

				for i1, v1 := range metric.LabeledMetrics {
					_, ok2 := rm.data.sum[metricSourceName].LabeledMetrics[v1.Name]
					if !ok2 {
						rm.data.sum[metricSourceName].LabeledMetrics[v1.Name] = &core.LabeledMetric{Name: v1.Name, Labels: v1.Labels, MetricValue: v1.MetricValue}
					} else {
						if i1 < len(s1.Value.(*core.MetricSet).LabeledMetrics) {
							if s1.Value.(*core.MetricSet).LabeledMetrics[i1].Name == v1.Name {
								rm.data.sum[metricSourceName].LabeledMetrics[v1.Name].IntValue = ((rm.data.sum[metricSourceName].LabeledMetrics[v1.Name].IntValue*int64(value.Len()) - s1.Value.(*core.MetricSet).LabeledMetrics[i1].IntValue + v1.IntValue) / int64(value.Len()))
							}
						}
					}
				}
				for k1, v1 := range metric.MetricValues {
					_, ok2 := rm.data.sum[metricSourceName].MetricValues[k1]
					if !ok2 {
						rm.data.sum[metricSourceName].MetricValues[k1] = &core.MetricValue{IntValue: v1.IntValue, FloatValue: v1.FloatValue, MetricType: v1.MetricType, ValueType: v1.ValueType}
					} else {
						_, ok2 := s1.Value.(*core.MetricSet).MetricValues[k1]
						if ok2 {
							rm.data.sum[metricSourceName].MetricValues[k1].IntValue = (rm.data.sum[metricSourceName].MetricValues[k1].IntValue*int64(value.Len()) - s1.Value.(*core.MetricSet).MetricValues[k1].IntValue + v1.IntValue) / int64(value.Len())
						}
					}
				}
				value.PushBack(metric)
			}
		}
	}
	//每30s统计一次
	if rm.loadResyncTime == 0 {
		rm.writeLoadFile()
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
		//处理数据，进行统计
		//rm.consumeData(data)
		rm.consumeData2Xlxs(data)

		//for k1,v1 :=range rm.data.historicalData{
		//	glog.Info("pod--"+k1+"的历史数据")
		//	for p1:= v1.Front();p1!=nil;p1=p1.Next(){
		//		for key,value := range p1.Value.(*core.MetricSet).MetricValues{
		//			glog.Info(key+"  :"+strconv.FormatInt(value.IntValue,10))
		//		}
		//		for _,value := range p1.Value.(*core.MetricSet).LabeledMetrics{
		//			glog.Info(value.Name+strconv.FormatInt(value.IntValue,10))
		//		}
		//	}
		//	glog.Info("-------统计数据")
		//	for key,value := range rm.data.sum[k1].MetricValues{
		//		glog.Info(key+"  :"+strconv.FormatInt(value.IntValue,10))
		//	}
		//	for _,value := range rm.data.sum[k1].LabeledMetrics{
		//		glog.Info(value.Name+strconv.FormatInt(value.IntValue,10))
		//	}
		//}

		//glog.Info("这里是数据------------------------------")
		//i:= 0
		//for key,value := range data.MetricSets{
		//	glog.Info("data"+strconv.Itoa(i)+": -----------")
		//	i+=1
		//	glog.Info("key: "+key)
		//	glog.Info("createTime: ")
		//	glog.Info(value.CreateTime)
		//	glog.Info("metric value: ")
		//	j:=0
		//	for key2,value2 := range value.MetricValues{
		//		glog.Info("subkey"+strconv.Itoa(j)+": "+key2+"-----------")
		//		j+=1
		//		glog.Info(value2.IntValue)
		//	}
		//	j=0
		//	glog.Info("labeled value: ")
		//	for _,value2 := range value.LabeledMetrics{
		//		glog.Info("subkey-l"+strconv.Itoa(j)+": -----------")
		//		j+=1
		//		glog.Info(value2.Name)
		//		glog.Info(value2.IntValue)
		//		glog.Info(value2.MetricType)
		//	}
		//	j=0
		//	glog.Info("labels: ")
		//	for key2,value2 := range value.Labels{
		//		glog.Info("subkey-l"+strconv.Itoa(j)+": "+key2+" -----------")
		//		j+=1
		//		glog.Info(value2)
		//	}
		//}
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

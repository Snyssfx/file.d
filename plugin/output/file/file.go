package file

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ozonru/file.d/cfg"
	"github.com/ozonru/file.d/fd"
	"github.com/ozonru/file.d/longpanic"
	"github.com/ozonru/file.d/pipeline"

	"go.uber.org/zap"
	"golang.org/x/net/context"
)

type Plugin struct {
	controller     pipeline.OutputPluginController
	logger         *zap.SugaredLogger
	config         *Config
	avgLogSize     int
	batcher        *pipeline.Batcher
	file           *os.File
	ctx            context.Context
	cancelFunc     context.CancelFunc
	idx            int
	nextSealUpTime time.Time

	targetDir     string
	fileExtension string
	fileName      string
	tsFileName    string

	SealUpCallback func(string)

	mu *sync.RWMutex
}

type data struct {
	outBuf []byte
}

const (
	fileNameSeparator = "_"
)

var FileSealUpInterval = time.Second

type Config struct {
	//> File name for log file.
	TargetFile string `json:"target_file" default:"/var/log/file-d.log"` //*

	//> Interval of creation new file
	RetentionInterval  cfg.Duration `json:"retention_interval" default:"1h" parse:"duration"` //*
	RetentionInterval_ time.Duration

	//> Layout is added to targetFile after sealing up. Determines result file name
	Layout string `json:"time_layout" default:"01-02-2006_15:04:05"` //*

	//> How much workers will be instantiated to send batches.
	WorkersCount  cfg.Expression `json:"workers_count" default:"gomaxprocs*4" parse:"expression"` //*
	WorkersCount_ int

	//> Maximum quantity of events to pack into one batch.
	BatchSize  cfg.Expression `json:"batch_size" default:"capacity/4" parse:"expression"` //*
	BatchSize_ int

	//> After this timeout batch will be sent even if batch isn't completed.
	BatchFlushTimeout  cfg.Duration `json:"batch_flush_timeout" default:"1s" parse:"duration"` //*
	BatchFlushTimeout_ time.Duration

	//> File mode for log files
	FileMode  cfg.Base8 `json:"file_mode" default:"0666" parse:"base8"` //*
	FileMode_ int64
}

func init() {
	fd.DefaultPluginRegistry.RegisterOutput(&pipeline.PluginStaticInfo{
		Type:    "file",
		Factory: Factory,
	})
}

func Factory() (pipeline.AnyPlugin, pipeline.AnyConfig) {
	return &Plugin{}, &Config{}
}

func (p *Plugin) Start(config pipeline.AnyConfig, params *pipeline.OutputPluginParams) {
	p.controller = params.Controller
	p.logger = params.Logger
	p.config = config.(*Config)

	dir, file := filepath.Split(p.config.TargetFile)
	p.targetDir = dir
	p.fileExtension = filepath.Ext(file)
	p.fileName = file[0 : len(file)-len(p.fileExtension)]
	p.tsFileName = "%s" + "-" + p.fileName

	p.batcher = pipeline.NewBatcher(
		params.PipelineName,
		"file",
		p.out,
		nil,
		p.controller,
		p.config.WorkersCount_,
		p.config.BatchSize_,
		p.config.BatchFlushTimeout_,
		0,
	)

	p.mu = &sync.RWMutex{}
	ctx, cancel := context.WithCancel(context.Background())
	p.ctx = ctx
	p.cancelFunc = cancel

	if err := os.MkdirAll(p.targetDir, os.ModePerm); err != nil {
		p.logger.Fatalf("could not create target dir: %s, error: %s", p.targetDir, err.Error())
	}

	p.idx = p.getStartIdx()
	p.createNew()
	p.setNextSealUpTime()

	if p.file == nil {
		p.logger.Panic("file struct is nil!")
	}
	if p.nextSealUpTime.IsZero() {
		p.logger.Panic("next seal up time is nil!")
	}

	longpanic.Go(p.fileSealUpTicker)
	p.batcher.Start()
}

func (p *Plugin) Stop() {
	p.cancelFunc()
	p.batcher.Stop()
}

func (p *Plugin) Out(event *pipeline.Event) {
	p.batcher.Add(event)
}

func (p *Plugin) out(workerData *pipeline.WorkerData, batch *pipeline.Batch) {
	if *workerData == nil {
		*workerData = &data{
			outBuf: make([]byte, 0, p.config.BatchSize_*p.avgLogSize),
		}
	}
	data := (*workerData).(*data)

	// handle to much memory consumption
	if cap(data.outBuf) > p.config.BatchSize_*p.avgLogSize {
		data.outBuf = make([]byte, 0, p.config.BatchSize_*p.avgLogSize)
	}

	outBuf := data.outBuf[:0]

	for _, event := range batch.Events {
		outBuf, _ = event.Encode(outBuf)
		outBuf = append(outBuf, byte('\n'))
	}
	data.outBuf = outBuf

	p.write(outBuf)
}

func (p *Plugin) fileSealUpTicker() {
	for {
		timer := time.NewTimer(time.Until(p.nextSealUpTime))
		select {
		case <-timer.C:
			p.sealUp()
		case <-p.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (p *Plugin) setNextSealUpTime() {
	ts := p.tsFileName[0 : len(p.tsFileName)-len(fileNameSeparator)-len(p.fileName)-len(p.fileExtension)]
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		p.logger.Panicf("coult nod convert timestamp to int for file: %s, error: %s", p.tsFileName, err.Error())
	}
	creationTime := time.Unix(t, 0)
	p.nextSealUpTime = creationTime.Add(p.config.RetentionInterval_)
}

func (p *Plugin) write(data []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, err := p.file.Write(data); err != nil {
		p.logger.Fatalf("could not write into the file: %s, error: %s", p.file.Name(), err.Error())
	}
}

func (p *Plugin) createNew() {
	p.tsFileName = fmt.Sprintf("%d%s%s%s", time.Now().Unix(), fileNameSeparator, p.fileName, p.fileExtension)
	f := fmt.Sprintf("%s%s", p.targetDir, p.tsFileName)
	pattern := fmt.Sprintf("%s*%s%s%s", p.targetDir, fileNameSeparator, p.fileName, p.fileExtension)
	matches, err := filepath.Glob(pattern)
	if len(matches) == 1 {
		p.tsFileName = path.Base(matches[0])
		f = fmt.Sprintf("%s%s", p.targetDir, p.tsFileName)
	}
	file, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_RDWR, os.FileMode(p.config.FileMode_))
	if err != nil {
		p.logger.Panicf("could not open or create file: %s, error: %s", f, err.Error())
	}
	p.file = file
}

// sealUp manages current file: renames, closes, and creates new.
func (p *Plugin) sealUp() {
	info, err := p.file.Stat()
	if err != nil {
		p.logger.Panicf("could not get info about file: %s, error: %s", p.file.Name(), err.Error())
	}
	if info.Size() == 0 {
		return
	}

	// newFileName will be like: ".var/log/log_1_01-02-2009_15:04.log
	newFileName := filepath.Join(p.targetDir, fmt.Sprintf("%s%s%d%s%s%s", p.fileName, fileNameSeparator, p.idx, fileNameSeparator, time.Now().Format(p.config.Layout), p.fileExtension))
	p.rename(newFileName)
	oldFile := p.file
	p.mu.Lock()
	p.createNew()
	p.nextSealUpTime = time.Now().Add(p.config.RetentionInterval_)
	p.mu.Unlock()
	if err := oldFile.Close(); err != nil {
		p.logger.Panicf("could not close file: %s, error: %s", oldFile.Name(), err.Error())
	}

	if p.SealUpCallback != nil {
		longpanic.Go(func() { p.SealUpCallback(newFileName) })
	}
}

func (p *Plugin) rename(newFileName string) {
	f := fmt.Sprintf("%s%s", p.targetDir, p.tsFileName)
	if err := os.Rename(f, newFileName); err != nil {
		p.logger.Panicf("could not rename file, error: %s", err.Error())
	}
	p.idx++
}

func (p *Plugin) getStartIdx() int {
	pattern := fmt.Sprintf("%s/%s%s*%s*%s", p.targetDir, p.fileName, fileNameSeparator, fileNameSeparator, p.fileExtension)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		p.logger.Panic(err.Error())
	}
	idx := -1
	for _, v := range matches {
		file := filepath.Base(v)
		i := file[len(p.fileName)+len(fileNameSeparator) : len(file)-len(p.fileExtension)-len(p.config.Layout)-len(fileNameSeparator)]
		maxIdx, err := strconv.Atoi(i)
		if err != nil {
			break
		}
		if maxIdx > idx {
			idx = maxIdx
		}
	}
	idx++
	return idx
}

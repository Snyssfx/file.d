// Package splunk is an output plugin that sends events to splunk database.
package splunk

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ozonru/file.d/cfg"
	"github.com/ozonru/file.d/fd"
	"github.com/ozonru/file.d/pipeline"
	insaneJSON "github.com/vitkovskii/insane-json"
	"go.uber.org/zap"
)

/*{ introduction
It sends events to splunk.
}*/

type Plugin struct {
	config         *Config
	logger         *zap.SugaredLogger
	avgLogSize     int
	batcher        *pipeline.Batcher
	controller     pipeline.OutputPluginController
	requestTimeout time.Duration
}

//! config-params
//^ config-params
type Config struct {
	//> @3@4@5@6
	//>
	//> A full URI address of splunk HEC endpoint. Format: `http://127.0.0.1:8088/services/collector`.
	Endpoint string `json:"endpoint" required:"true"` //*

	//> @3@4@5@6
	//>
	//> Token for an authentication for a HEC endpoint.
	Token string `json:"token" required:"true"` //*

	//> @3@4@5@6
	//>
	//> How many workers will be instantiated to send batches.
	WorkersCount  cfg.Expression `json:"workers_count" default:"gomaxprocs*4" parse:"expression"` //*
	WorkersCount_ int

	//> @3@4@5@6
	//>
	//> Client timeout when sends requests to HTTP Event Collector.
	RequestTimeout  cfg.Duration `json:"request_timeout" default:"1s" parse:"duration"` //*
	RequestTimeout_ time.Duration

	//> @3@4@5@6
	//>
	//> A maximum quantity of events to pack into one batch.
	BatchSize  cfg.Expression `json:"batch_size" default:"capacity/4" parse:"expression"` //*
	BatchSize_ int

	//> @3@4@5@6
	//>
	//> After this timeout the batch will be sent even if batch isn't completed.
	BatchFlushTimeout  cfg.Duration `json:"batch_flush_timeout" default:"200ms" parse:"duration"` //*
	BatchFlushTimeout_ time.Duration
}

type data struct {
	outBuf []byte
}

func init() {
	fd.DefaultPluginRegistry.RegisterOutput(&pipeline.PluginStaticInfo{
		Type:    "splunk",
		Factory: Factory,
	})
}

func Factory() (pipeline.AnyPlugin, pipeline.AnyConfig) {
	return &Plugin{}, &Config{}
}

func (p *Plugin) Start(config pipeline.AnyConfig, params *pipeline.OutputPluginParams) {
	p.controller = params.Controller
	p.logger = params.Logger
	p.avgLogSize = params.PipelineSettings.AvgLogSize
	p.config = config.(*Config)

	p.batcher = pipeline.NewBatcher(
		params.PipelineName,
		"splunk",
		p.out,
		p.maintenance,
		p.controller,
		p.config.WorkersCount_,
		p.config.BatchSize_,
		p.config.BatchFlushTimeout_,
		0,
	)
	p.batcher.Start()
}

func (p *Plugin) Stop() {
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
		root := insaneJSON.Spawn()
		root.AddField("event").MutateToNode(event.Root.Node)
		outBuf = root.Encode(outBuf)
	}
	data.outBuf = outBuf

	for {
		err := p.send(outBuf, p.config.RequestTimeout_)
		if err != nil {
			p.logger.Errorf("can't send data to splunk address=%s: %s", p.config.Endpoint, err.Error())
			time.Sleep(time.Second)

			continue
		}

		break
	}
}

func (p *Plugin) maintenance(workerData *pipeline.WorkerData) {}

func (p *Plugin) send(data []byte, timeout time.Duration) error {
	c := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	r := bytes.NewReader(data)
	req, err := http.NewRequestWithContext(context.Background(), "POST", p.config.Endpoint, r)
	if err != nil {
		return fmt.Errorf("can't create request: %w", err)
	}

	req.Header.Set("Authorization", "Splunk "+p.config.Token)
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("can't send request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("can't read response: %w", err)
	}

	root, err := insaneJSON.DecodeBytes(b)
	if err != nil {
		return fmt.Errorf("can't decode response: %w", err)
	}

	code := root.Dig("code").AsInt()
	if code > 0 {
		return fmt.Errorf("error while sending to splunk: %s", string(b))
	}

	return nil
}

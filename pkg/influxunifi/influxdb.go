// Package influxunifi provides the methods to turn UniFi measurements into influx
// data-points with appropriate tags and fields.
package influxunifi

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	influx "github.com/influxdata/influxdb-client-go/v2"
	influxV1 "github.com/influxdata/influxdb1-client/v2"
	"github.com/unpoller/unifi"
	"github.com/unpoller/unpoller/pkg/poller"
	"github.com/unpoller/unpoller/pkg/webserver"
	"golift.io/cnfg"
)

// PluginName is the name of this plugin.
const PluginName = "influxdb"

const (
	defaultInterval     = 30 * time.Second
	minimumInterval     = 10 * time.Second
	defaultInfluxDB     = "unifi"
	defaultInfluxUser   = "unifipoller"
	defaultInfluxOrg    = "unifi"
	defaultInfluxBucket = "unifi"
	defaultInfluxURL    = "http://127.0.0.1:8086"
)

// Config defines the data needed to store metrics in InfluxDB.
type Config struct {
	Interval cnfg.Duration `json:"interval,omitempty" toml:"interval,omitempty" xml:"interval" yaml:"interval"`
	Version2 bool          `json:"version2,omitempty" toml:"version2,omitempty" xml:"version2" yaml:"version2"`

	// V1
	User string `json:"user,omitempty" toml:"user,omitempty" xml:"user" yaml:"user"`
	Pass string `json:"pass,omitempty" toml:"pass,omitempty" xml:"pass" yaml:"pass"`
	DB   string `json:"db,omitempty" toml:"db,omitempty" xml:"db" yaml:"db"`

	// V2 features
	AuthToken string `json:"auth_token,omitempty" toml:"auth_token,omitempty" xml:"auth_token" yaml:"auth_token"`
	Org       string `json:"org,omitempty" toml:"org,omitempty" xml:"org" yaml:"org"`
	Bucket    string `json:"bucket,omitempty" toml:"bucket,omitempty" xml:"bucket" yaml:"bucket"`
	BatchSize uint   `json:"batch_size,omitempty" toml:"batch_size,omitempty" xml:"batch_size" yaml:"batch_size"`

	// Common
	URL       string `json:"url,omitempty" toml:"url,omitempty" xml:"url" yaml:"url"`
	Disable   bool   `json:"disable" toml:"disable" xml:"disable,attr" yaml:"disable"`
	VerifySSL bool   `json:"verify_ssl" toml:"verify_ssl" xml:"verify_ssl" yaml:"verify_ssl"`
	// Save data for dead ports? ie. ports that are down or disabled.
	DeadPorts bool `json:"dead_ports" toml:"dead_ports" xml:"dead_ports" yaml:"dead_ports"`
}

// InfluxDB allows the data to be nested in the config file.
type InfluxDB struct {
	*Config `json:"influxdb" toml:"influxdb" xml:"influxdb" yaml:"influxdb"`
}

// InfluxUnifi is returned by New() after you provide a Config.
type InfluxUnifi struct {
	Collector poller.Collect
	influxV1  influxV1.Client
	influxV2  influx.ClientInterface
	LastCheck time.Time
	*InfluxDB
}

type metric struct {
	Table  string
	Tags   map[string]string
	Fields map[string]any
	TS     time.Time
}

func init() { // nolint: gochecknoinits
	u := &InfluxUnifi{InfluxDB: &InfluxDB{}, LastCheck: time.Now()}

	poller.NewOutput(&poller.Output{
		Name:   PluginName,
		Config: u.InfluxDB,
		Method: u.Run,
	})
}

// PollController runs forever, polling UniFi and pushing to InfluxDB
// This is started by Run() or RunBoth() after everything checks out.
func (u *InfluxUnifi) PollController() {
	interval := u.Interval.Round(time.Second)
	ticker := time.NewTicker(interval)
	log.Printf("[INFO] Poller->InfluxDB started, interval: %v, dp: %v, db: %s, url: %s",
		interval, u.DeadPorts, u.DB, u.URL)

	for u.LastCheck = range ticker.C {
		metrics, err := u.Collector.Metrics(&poller.Filter{Name: "unifi"})
		if err != nil {
			u.LogErrorf("metric fetch for InfluxDB failed: %v", err)
			continue
		}

		events, err := u.Collector.Events(&poller.Filter{Name: "unifi", Dur: interval})
		if err != nil {
			u.LogErrorf("event fetch for InfluxDB failed: %v", err)
			continue
		}

		report, err := u.ReportMetrics(metrics, events)
		if err != nil {
			// XXX: reset and re-auth? not sure..
			u.LogErrorf("%v", err)
			continue
		}

		u.Logf("UniFi Metrics Recorded. %v", report)
	}
}

// Run runs a ticker to poll the unifi server and update influxdb.
func (u *InfluxUnifi) Run(c poller.Collect) error {
	var err error

	if u.Collector = c; u.Config == nil || u.Disable {
		u.Logf("InfluxDB config missing (or disabled), InfluxDB output disabled!")
		return nil
	}

	u.setConfigDefaults()

	if u.Config.Version2 {
		// we're a version 2
		tlsConfig := &tls.Config{InsecureSkipVerify: !u.VerifySSL} // nolint: gosec
		serverOptions := influx.DefaultOptions().SetTLSConfig(tlsConfig).SetBatchSize(u.BatchSize)
		u.influxV2 = influx.NewClientWithOptions(u.URL, u.AuthToken, serverOptions)
	} else {
		u.influxV1, err = influxV1.NewHTTPClient(influxV1.HTTPConfig{
			Addr:      u.URL,
			Username:  u.User,
			Password:  u.Pass,
			TLSConfig: &tls.Config{InsecureSkipVerify: !u.VerifySSL}, // nolint: gosec
		})
		if err != nil {
			return fmt.Errorf("making client: %w", err)
		}
	}

	fake := *u.Config
	fake.Pass = strconv.FormatBool(fake.Pass != "")

	webserver.UpdateOutput(&webserver.Output{Name: PluginName, Config: fake})
	u.PollController()

	return nil
}

func (u *InfluxUnifi) setConfigDefaults() {
	if u.URL == "" {
		u.URL = defaultInfluxURL
	}

	if strings.HasPrefix(u.AuthToken, "file://") {
		u.AuthToken = u.getPassFromFile(strings.TrimPrefix(u.AuthToken, "file://"))
	}

	if u.AuthToken == "" {
		u.AuthToken = "anonymous"
	}

	if u.Org == "" {
		u.Org = defaultInfluxOrg
	}

	if u.Bucket == "" {
		u.Bucket = defaultInfluxBucket
	}

	if u.BatchSize == 0 {
		u.BatchSize = 20
	}

	if u.User == "" {
		u.User = defaultInfluxUser
	}

	if strings.HasPrefix(u.Pass, "file://") {
		u.Pass = u.getPassFromFile(strings.TrimPrefix(u.Pass, "file://"))
	}

	if u.Pass == "" {
		u.Pass = defaultInfluxUser
	}

	if u.DB == "" {
		u.DB = defaultInfluxDB
	}

	if u.Interval.Duration == 0 {
		u.Interval = cnfg.Duration{Duration: defaultInterval}
	} else if u.Interval.Duration < minimumInterval {
		u.Interval = cnfg.Duration{Duration: minimumInterval}
	}

	u.Interval = cnfg.Duration{Duration: u.Interval.Duration.Round(time.Second)}
}

func (u *InfluxUnifi) getPassFromFile(filename string) string {
	b, err := os.ReadFile(filename)
	if err != nil {
		u.LogErrorf("Reading InfluxDB Password File: %v", err)
	}

	return strings.TrimSpace(string(b))
}

// ReportMetrics batches all device and client data into influxdb data points.
// Call this after you've collected all the data you care about.
// Returns an error if influxdb calls fail, otherwise returns a report.
func (u *InfluxUnifi) ReportMetrics(m *poller.Metrics, e *poller.Events) (*Report, error) {
	r := &Report{
		UseV2:   u.Config.Version2,
		Metrics: m,
		Events:  e,
		ch:      make(chan *metric),
		Start:   time.Now(),
		Counts:  &Counts{Val: make(map[item]int)},
	}
	defer close(r.ch)

	if u.Config.Version2 {
		// Make a new Influx Points Batcher.
		r.writer = &u.influxV2.WriteAPI(u.Org, u.Bucket)

		go u.collect(r, r.ch)
		// Batch all the points.
		u.loopPoints(r)
		r.wg.Wait() // wait for all points to finish batching!

		// Flush all the points.
		r.writer.Flush()
	} else {
		var err error

		// Make a new Influx Points Batcher.
		r.bp, err = influxV1.NewBatchPoints(influxV1.BatchPointsConfig{Database: u.DB})

		if err != nil {
			return nil, fmt.Errorf("influx.NewBatchPoint: %w", err)
		}

		go u.collect(r, r.ch)
		// Batch all the points.
		u.loopPoints(r)
		r.wg.Wait() // wait for all points to finish batching!

		// Send all the points.
		if err = u.influxV1.Write(r.bp); err != nil {
			return nil, fmt.Errorf("influxdb.Write(points): %w", err)
		}
	}

	r.Elapsed = time.Since(r.Start)

	return r, nil
}

// collect runs in a go routine and batches all the points.
func (u *InfluxUnifi) collect(r report, ch chan *metric) {
	for m := range ch {
		if m.TS.IsZero() {
			m.TS = r.metrics().TS
		}

		if u.Config.Version2 {
			pt := influx.NewPoint(m.Table, m.Tags, m.Fields, m.TS)
			r.batchV2(m, pt)
		} else {
			pt, err := influxV1.NewPoint(m.Table, m.Tags, m.Fields, m.TS)
			if err == nil {
				r.batchV1(m, pt)
			}

			r.error(err)
		}
		r.done()
	}
}

// loopPoints kicks off 3 or 7 go routines to process metrics and send them
// to the collect routine through the metric channel.
func (u *InfluxUnifi) loopPoints(r report) {
	m := r.metrics()

	for _, s := range m.RogueAPs {
		u.switchExport(r, s)
	}

	for _, s := range m.Sites {
		u.switchExport(r, s)
	}

	for _, s := range m.SitesDPI {
		u.batchSiteDPI(r, s)
	}

	for _, s := range m.Clients {
		u.switchExport(r, s)
	}

	for _, s := range m.Devices {
		u.switchExport(r, s)
	}

	for _, s := range r.events().Logs {
		u.switchExport(r, s)
	}

	appTotal := make(totalsDPImap)
	catTotal := make(totalsDPImap)

	for _, s := range m.ClientsDPI {
		u.batchClientDPI(r, s, appTotal, catTotal)
	}

	reportClientDPItotals(r, appTotal, catTotal)
}

func (u *InfluxUnifi) switchExport(r report, v any) { //nolint:cyclop
	switch v := v.(type) {
	case *unifi.RogueAP:
		u.batchRogueAP(r, v)
	case *unifi.UAP:
		u.batchUAP(r, v)
	case *unifi.USW:
		u.batchUSW(r, v)
	case *unifi.USG:
		u.batchUSG(r, v)
	case *unifi.UXG:
		u.batchUXG(r, v)
	case *unifi.UDM:
		u.batchUDM(r, v)
	case *unifi.Site:
		u.batchSite(r, v)
	case *unifi.Client:
		u.batchClient(r, v)
	case *unifi.Event:
		u.batchEvent(r, v)
	case *unifi.IDS:
		u.batchIDS(r, v)
	case *unifi.Alarm:
		u.batchAlarms(r, v)
	case *unifi.Anomaly:
		u.batchAnomaly(r, v)
	default:
		u.LogErrorf("invalid export type: %T", v)
	}
}

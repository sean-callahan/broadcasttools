package broadcasttools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type sensorType string

const (
	sensorTemp   sensorType = "temp"
	sensorMeter  sensorType = "meter"
	sensorVC     sensorType = "vc"
	sensorStatus sensorType = "status"
	sensorRelay  sensorType = "relay"
)

type parser func(src map[string]interface{}, index int) (interface{}, sensorType)

var (
	regexpTemp   = regexp.MustCompile(`^T1(\d+)$`)
	regexpMeter  = regexp.MustCompile(`^M1(\d+)$`)
	regexpVC     = regexp.MustCompile(`^VCLabel(\d+)$`)
	regexpStatus = regexp.MustCompile(`^S1(\d+)$`)
	regexpRelay  = regexp.MustCompile(`^R2(\d+)$`)

	parsers = map[*regexp.Regexp]parser{
		regexpTemp: func(src map[string]interface{}, index int) (interface{}, sensorType) {
			t := src[fmt.Sprintf("TempValue%02d", index)].(string)
			t = strings.TrimSuffix(t, " *F")
			v, _ := strconv.Atoi(t)
			return v, sensorTemp
		},
		regexpMeter: func(src map[string]interface{}, index int) (interface{}, sensorType) {
			return src[fmt.Sprintf("MeterValue%02d", index)], sensorMeter
		},
		regexpVC: func(src map[string]interface{}, index int) (interface{}, sensorType) {
			return src[fmt.Sprintf("VCValue%02d", index)], sensorVC
		},
		regexpStatus: func(src map[string]interface{}, index int) (interface{}, sensorType) {
			return src[fmt.Sprintf("StatusIndicator%02d", index)], sensorStatus
		},
		regexpRelay: func(src map[string]interface{}, index int) (interface{}, sensorType) {
			return src[fmt.Sprintf("RelayIndicator%02d", index)], sensorRelay
		},
	}
)

type BroadcastTools struct {
	Servers  []string
	User     string
	Password string

	devices     []Device
	initialized bool
	rnd         *rand.Rand
}

type Device interface {
	Dial() error
	Close() error
	Gather(acc telegraf.Accumulator) error
}

const sampleConfig = `
  ## An array of URLs to gather stats from. i.e.,
  ##   http://example.com:3000
  servers = ["http://localhost:8080"]
  ## Username
  user = "admin"
  ## Password
  password = "password"
`

func (bt *BroadcastTools) init() error {
	if bt.initialized {
		return nil
	}

	bt.rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, u := range bt.Servers {
		base, err := url.Parse(u)
		if err != nil {
			return err
		}

		d := &device{
			bt:   bt,
			base: base,
			c: &http.Client{
				Timeout: time.Minute,
			},
		}
		if err := d.Dial(); err != nil {
			return err
		}

		bt.devices = append(bt.devices, d)
	}

	bt.initialized = true
	return nil
}

func (bt *BroadcastTools) SampleConfig() string {
	return sampleConfig
}

func (bt *BroadcastTools) Description() string {
	return "Read metrics from one or many Broadcast Tools devices"
}

func (bt *BroadcastTools) Gather(acc telegraf.Accumulator) error {
	if !bt.initialized {
		if err := bt.init(); err != nil {
			return err
		}
	}

	var wg sync.WaitGroup

	for _, device := range bt.devices {
		wg.Add(1)

		go func(d Device, a telegraf.Accumulator) {
			defer wg.Done()
			if err := d.Gather(a); err != nil {
				acc.AddError(err)
			}
		}(device, acc)
	}

	wg.Wait()
	return nil
}

type device struct {
	bt   *BroadcastTools
	base *url.URL
	c    *http.Client
	ck   *http.Cookie
}

func (d device) send(method string, path string, data io.Reader, sendCookie bool) (*http.Response, error) {
	u := *d.base
	u.Path = path

	r, err := http.NewRequest(method, u.String(), data)
	if err != nil {
		return nil, err
	}
	if sendCookie {
		r.AddCookie(d.ck)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	return d.c.Do(r)
}

func (d *device) Dial() error {
	if d.ck != nil {
		return errors.New("already logged in")
	}

	v := url.Values{}
	v.Set("AccessVal", strconv.Itoa(d.bt.rnd.Intn(1000)))
	v.Set("LoginUser", d.bt.User)
	v.Set("LoginPass", d.bt.Password)

	r, err := d.send(http.MethodPost, "/cgi-bin/postauth.cgi", strings.NewReader(v.Encode()), false)
	if err != nil {
		return err
	}
	if r.StatusCode != http.StatusOK {
		return errors.New("authentication failed")
	}

	cks := r.Cookies()
	if len(cks) < 1 {
		return errors.New("no cookies returned")
	}
	d.ck = cks[0]

	return nil
}

func (d device) Close() error {
	v := url.Values{}
	v.Set("Logout", "1")

	_, err := d.send(http.MethodPost, "/cgi-bin/postlogout.cgi", strings.NewReader(v.Encode()), true)
	if err != nil {
		return nil // ignore logout errors
	}

	d.ck = nil
	d.c = nil
	return nil
}

func (d *device) Gather(acc telegraf.Accumulator) error {
	r, err := d.send(http.MethodGet, "/cgi-bin/getexchanger_monitor.cgi", nil, true)
	if err != nil {
		return err
	}
	if r.StatusCode != http.StatusOK {
		if r.StatusCode == http.StatusPartialContent { // cookie invalid
			if err := d.Dial(); err != nil { // reconnect
				return err
			}
		}
		return fmt.Errorf("expected status %d; got %d", http.StatusOK, r.StatusCode)
	}
	defer r.Body.Close()

	var data map[string]interface{}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return err
	}

	values := data["values"].(map[string]interface{})

	fields := make(map[string]interface{})

	for key := range values {
		for reg, parser := range parsers {
			matches := reg.FindStringSubmatch(key)
			if len(matches) < 2 {
				continue
			}
			index, err := strconv.Atoi(matches[1])
			if err != nil {
				continue
			}
			value, sensor := parser(values, index)
			fields[fmt.Sprintf("%s_%d", sensor, index)] = value
		}
	}

	acc.AddFields("broadcasttools", fields, nil)

	return nil
}

func init() {
	inputs.Add("broadcasttools", func() telegraf.Input {
		bt := &BroadcastTools{}
		return bt
	})
}

package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/utils"
)

const (
	versionMimetype = "appplication/vnd.docker.plugins.v1+json"
	defaultTimeOut  = 120
)

func NewClient(addr string) *Client {
	// No TLS. Hopefully this discourages non-local plugins
	tr := &http.Transport{}
	protoAndAddr := strings.Split(addr, "://")
	utils.ConfigureTCPTransport(tr, protoAndAddr[0], protoAndAddr[1])
	return &Client{&http.Client{Transport: tr}, protoAndAddr[1]}
}

type Client struct {
	http *http.Client
	addr string
}

func (c *Client) Call(serviceMethod string, args interface{}, ret interface{}) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(args); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "/"+serviceMethod, &buf)
	if err != nil {
		return err
	}
	req.Header.Add("Accept", versionMimetype)
	req.URL.Scheme = "http"
	req.URL.Host = c.addr

	var retries int
	start := time.Now()

	for {
		resp, err := c.http.Do(req)
		if err != nil {
			timeOff := backoff(retries)
			if timeOff+time.Since(start) > defaultTimeOut {
				return err
			}
			retries++
			logrus.Warn("Unable to connect to plugin: %s, retrying in %ds\n", c.addr, timeOff)
			time.Sleep(timeOff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			remoteErr, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil
			}
			return fmt.Errorf("Plugin Error: %s", remoteErr)
		}

		return json.NewDecoder(resp.Body).Decode(&ret)
	}
}

func backoff(retries int) time.Duration {
	b, max := float64(1), float64(defaultTimeOut)
	for b < max && retries > 0 {
		b *= 2
		retries--
	}
	if b > max {
		b = max
	}
	return time.Duration(b)
}

package main

import (
	"encoding/json"
	"github.com/Azure/azure-sdk-for-go/profiles/latest/network/mgmt/network"
	"github.com/Azure/go-autorest/autorest/to"
	scanner "github.com/anvie/port-scanner"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/remeh/sizedwaitgroup"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"strconv"
	"sync"
	"time"
)

type PortscannerResult struct {
	IpAddress string
	Labels    prometheus.Labels
	Value     float64
}

type Portscanner struct {
	List      map[string][]PortscannerResult
	PublicIps map[string]network.PublicIPAddress
	Enabled   bool `json:"-"`
	mux       sync.Mutex

	logger *log.Entry

	Callbacks struct {
		StartupScan        func(c *Portscanner)
		FinishScan         func(c *Portscanner)
		StartScanIpAdress  func(c *Portscanner, pip network.PublicIPAddress)
		FinishScanIpAdress func(c *Portscanner, pip network.PublicIPAddress, elapsed float64)
		ResultCleanup      func(c *Portscanner)
		ResultPush         func(c *Portscanner, result PortscannerResult)
	} `json:"-"`
}

func (c *Portscanner) Init() {
	c.Enabled = false
	c.List = map[string][]PortscannerResult{}
	c.PublicIps = map[string]network.PublicIPAddress{}

	c.logger = log.WithField("component", "portscanner")

	c.Callbacks.StartupScan = func(c *Portscanner) {}
	c.Callbacks.FinishScan = func(c *Portscanner) {}
	c.Callbacks.StartScanIpAdress = func(c *Portscanner, pip network.PublicIPAddress) {}
	c.Callbacks.FinishScanIpAdress = func(c *Portscanner, pip network.PublicIPAddress, elapsed float64) {}
	c.Callbacks.ResultCleanup = func(c *Portscanner) {}
	c.Callbacks.ResultPush = func(c *Portscanner, result PortscannerResult) {}
}

func (c *Portscanner) Enable() {
	c.Enabled = true
}

func (c *Portscanner) CacheLoad(path string) {
	c.mux.Lock()

	file, err := os.Open(path) // #nosec
	if err != nil {
		c.logger.Panic(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			c.logger.Errorf("error closing file: %s\n", err)
		}
	}()

	jsonContent, _ := ioutil.ReadAll(file)
	err = json.Unmarshal(jsonContent, &c)
	if err != nil {
		c.logger.Errorf("failed to load portscanner cache: %v", err)
	}

	c.mux.Unlock()

	// cleanup and update prometheus again
	c.Cleanup()
	c.Publish()
}

func (c *Portscanner) CacheSave(path string) {
	c.mux.Lock()

	jsonData, _ := json.Marshal(c)
	err := ioutil.WriteFile(path, jsonData, 0600)
	if err != nil {
		c.logger.Panic(err)
	}

	c.mux.Unlock()
}

func (c *Portscanner) SetAzurePublicIpList(pipList []network.PublicIPAddress) {
	c.mux.Lock()

	// build map
	ipAddressList := map[string]network.PublicIPAddress{}
	for _, pip := range pipList {
		ipAddress := to.String(pip.IPAddress)
		ipAddressList[ipAddress] = pip
	}

	c.PublicIps = ipAddressList
	c.mux.Unlock()
}

func (c *Portscanner) addResults(pip network.PublicIPAddress, results []PortscannerResult) {
	ipAddress := to.String(pip.IPAddress)
	// update result cache and update prometheus
	c.mux.Lock()
	c.List[ipAddress] = results
	c.pushResults()
	c.mux.Unlock()
}

func (c *Portscanner) Cleanup() {
	// cleanup
	c.mux.Lock()

	orphanedIpList := []string{}
	for ipAddress := range c.List {
		if _, ok := c.PublicIps[ipAddress]; !ok {
			orphanedIpList = append(orphanedIpList, ipAddress)
		}
	}

	// delete oprhaned IPs
	for _, ipAddress := range orphanedIpList {
		delete(c.List, ipAddress)
	}

	c.mux.Unlock()
}

func (c *Portscanner) Publish() {
	c.mux.Lock()
	c.Callbacks.ResultCleanup(c)
	c.pushResults()
	c.mux.Unlock()
}

func (c *Portscanner) pushResults() {
	for _, results := range c.List {
		for _, result := range results {
			c.Callbacks.ResultPush(c, result)
		}
	}
}

func (c *Portscanner) Start() {
	portscanTimeout := time.Duration(opts.Portscan.Timeout) * time.Second

	c.Callbacks.StartupScan(c)

	// cleanup and update prometheus again
	c.Cleanup()
	c.Publish()

	swg := sizedwaitgroup.New(opts.Portscan.Parallel)
	for _, pip := range c.PublicIps {
		swg.Add()
		go func(pip network.PublicIPAddress, portscanTimeout time.Duration) {
			defer swg.Done()

			c.Callbacks.StartScanIpAdress(c, pip)

			results, elapsed := c.scanIp(pip, portscanTimeout)

			c.Callbacks.FinishScanIpAdress(c, pip, elapsed)

			c.addResults(pip, results)
		}(pip, portscanTimeout)
	}

	// wait for all port scanners
	swg.Wait()

	// cleanup and update prometheus again
	c.Cleanup()
	c.Publish()

	c.Callbacks.FinishScan(c)
}

func (c *Portscanner) scanIp(pip network.PublicIPAddress, portscanTimeout time.Duration) (result []PortscannerResult, elapsed float64) {
	ipAddress := to.String(pip.IPAddress)
	startTime := time.Now().Unix()

	contextLogger := c.logger.WithField("ipAddress", ipAddress)

	// check if public ip is still owned
	if _, ok := c.PublicIps[ipAddress]; !ok {
		return
	}

	ps := scanner.NewPortScanner(ipAddress, portscanTimeout, opts.Portscan.Threads)

	for _, portrange := range portscanPortRange {
		openedPorts := ps.GetOpenedPort(portrange.FirstPort, portrange.LastPort)

		for _, port := range openedPorts {
			contextLogger.WithField("port", port).Debugf("detected open port %v", port)
			result = append(
				result,
				PortscannerResult{
					IpAddress: ipAddress,
					Labels: prometheus.Labels{
						"ipAddress":   ipAddress,
						"protocol":    "TCP",
						"port":        strconv.Itoa(port),
						"description": "",
					},
					Value: 1,
				},
			)
		}
	}

	elapsed = float64(time.Now().Unix() - startTime)

	return result, elapsed
}

// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package uyuni

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/kolo/xmlrpc"
	"github.com/pkg/errors"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/refresh"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

const (
	uyuniXMLRPCAPIPath = "/rpc/api"

	uyuniMetaLabelPrefix     = model.MetaLabelPrefix + "uyuni_"
	uyuniLabelMinionHostname = uyuniMetaLabelPrefix + "minion_hostname"
	uyuniLabelPrimaryFQDN    = uyuniMetaLabelPrefix + "primary_fqdn"
	uyuniLablelSystemID      = uyuniMetaLabelPrefix + "system_id"
	uyuniLablelGroups        = uyuniMetaLabelPrefix + "groups"
	uyuniLablelEndpointName  = uyuniMetaLabelPrefix + "endpoint_name"
	uyuniLablelExporter      = uyuniMetaLabelPrefix + "exporter"
	uyuniLabelProxyModule    = uyuniMetaLabelPrefix + "proxy_module"
	uyuniLabelMetricsPath    = uyuniMetaLabelPrefix + "metrics_path"
	uyuniLabelScheme         = uyuniMetaLabelPrefix + "scheme"
)

// DefaultSDConfig is the default Uyuni SD configuration.
var DefaultSDConfig = SDConfig{
	Entitlement:     "monitoring_entitled",
	Separator:       ",",
	RefreshInterval: model.Duration(1 * time.Minute),
}

func init() {
	discovery.RegisterConfig(&SDConfig{})
}

// SDConfig is the configuration for Uyuni based service discovery.
type SDConfig struct {
	Server           config.URL              `yaml:"server"`
	Username         string                  `yaml:"username"`
	Password         config.Secret           `yaml:"password"`
	HTTPClientConfig config.HTTPClientConfig `yaml:",inline"`
	Entitlement      string                  `yaml:"entitlement,omitempty"`
	Separator        string                  `yaml:"separator,omitempty"`
	RefreshInterval  model.Duration          `yaml:"refresh_interval,omitempty"`
}

type systemGroupID struct {
	GroupID   int    `xmlrpc:"id"`
	GroupName string `xmlrpc:"name"`
}

type networkInfo struct {
	SystemID    int    `xmlrpc:"system_id"`
	Hostname    string `xmlrpc:"hostname"`
	PrimaryFQDN string `xmlrpc:"primary_fqdn"`
	IP          string `xmlrpc:"ip"`
}

type endpointInfo struct {
	SystemID     int    `xmlrpc:"system_id"`
	EndpointName string `xmlrpc:"endpoint_name"`
	Port         int    `xmlrpc:"port"`
	Path         string `xmlrpc:"path"`
	Module       string `xmlrpc:"module"`
	ExporterName string `xmlrpc:"exporter_name"`
	TLSEnabled   bool   `xmlrpc:"tls_enabled"`
}

// Discovery periodically performs Uyuni API requests. It implements the Discoverer interface.
type Discovery struct {
	*refresh.Discovery
	apiURL       *url.URL
	roundTripper http.RoundTripper
	username     string
	password     string
	entitlement  string
	separator    string
	interval     time.Duration
	logger       log.Logger
}

// Name returns the name of the Config.
func (*SDConfig) Name() string { return "uyuni" }

// NewDiscoverer returns a Discoverer for the Config.
func (c *SDConfig) NewDiscoverer(opts discovery.DiscovererOptions) (discovery.Discoverer, error) {
	return NewDiscovery(c, opts.Logger)
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))

	if err != nil {
		return err
	}
	if c.Server.URL == nil {
		return errors.New("Uyuni SD configuration requires server host")
	}

	_, err = url.Parse(c.Server.String())
	if err != nil {
		return errors.Wrap(err, "Uyuni Server URL is not valid")
	}

	if c.Username == "" {
		return errors.New("Uyuni SD configuration requires a username")
	}
	if c.Password == "" {
		return errors.New("Uyuni SD configuration requires a password")
	}
	return nil
}

func login(rpcclient *xmlrpc.Client, user string, pass string) (string, error) {
	var result string
	err := rpcclient.Call("auth.login", []interface{}{user, pass}, &result)
	return result, err
}

func logout(rpcclient *xmlrpc.Client, token string) error {
	return rpcclient.Call("auth.logout", token, nil)
}

func getSystemGroupsInfoOfMonitoredClients(rpcclient *xmlrpc.Client, token string, entitlement string) (map[int][]systemGroupID, error) {
	var systemGroupsInfos []struct {
		SystemID     int             `xmlrpc:"id"`
		SystemGroups []systemGroupID `xmlrpc:"system_groups"`
	}

	err := rpcclient.Call("system.listSystemGroupsForSystemsWithEntitlement", []interface{}{token, entitlement}, &systemGroupsInfos)
	if err != nil {
		return nil, err
	}

	result := make(map[int][]systemGroupID)
	for _, systemGroupsInfo := range systemGroupsInfos {
		result[systemGroupsInfo.SystemID] = systemGroupsInfo.SystemGroups
	}
	return result, nil
}

func getNetworkInformationForSystems(rpcclient *xmlrpc.Client, token string, systemIDs []int) (map[int]networkInfo, error) {
	var networkInfos []networkInfo
	err := rpcclient.Call("system.getNetworkForSystems", []interface{}{token, systemIDs}, &networkInfos)
	if err != nil {
		return nil, err
	}

	result := make(map[int]networkInfo)
	for _, networkInfo := range networkInfos {
		result[networkInfo.SystemID] = networkInfo
	}
	return result, nil
}

func getEndpointInfoForSystems(
	rpcclient *xmlrpc.Client,
	token string,
	systemIDs []int,
) ([]endpointInfo, error) {
	var endpointInfos []endpointInfo
	err := rpcclient.Call(
		"system.monitoring.listEndpoints",
		[]interface{}{token, systemIDs}, &endpointInfos)
	if err != nil {
		return nil, err
	}
	return endpointInfos, err
}

// NewDiscovery returns a uyuni discovery for the given configuration.
func NewDiscovery(conf *SDConfig, logger log.Logger) (*Discovery, error) {
	var apiURL *url.URL
	*apiURL = *conf.Server.URL
	apiURL.Path = path.Join(apiURL.Path, uyuniXMLRPCAPIPath)

	rt, err := config.NewRoundTripperFromConfig(conf.HTTPClientConfig, "uyuni_sd")
	if err != nil {
		return nil, err
	}

	d := &Discovery{
		apiURL:       apiURL,
		roundTripper: rt,
		username:     conf.Username,
		password:     string(conf.Password),
		entitlement:  conf.Entitlement,
		separator:    conf.Separator,
		interval:     time.Duration(conf.RefreshInterval),
		logger:       logger,
	}

	d.Discovery = refresh.NewDiscovery(
		logger,
		"uyuni",
		time.Duration(conf.RefreshInterval),
		d.refresh,
	)
	return d, nil
}

func (d *Discovery) getEndpointLabels(
	endpoint endpointInfo,
	systemGroupIDs []systemGroupID,
	networkInfo networkInfo,
) model.LabelSet {

	var addr, scheme string
	managedGroupNames := getSystemGroupNames(systemGroupIDs)
	addr = fmt.Sprintf("%s:%d", networkInfo.Hostname, endpoint.Port)
	if endpoint.TLSEnabled {
		scheme = "https"
	} else {
		scheme = "http"
	}

	result := model.LabelSet{
		model.AddressLabel:       model.LabelValue(addr),
		uyuniLabelMinionHostname: model.LabelValue(networkInfo.Hostname),
		uyuniLabelPrimaryFQDN:    model.LabelValue(networkInfo.PrimaryFQDN),
		uyuniLablelSystemID:      model.LabelValue(fmt.Sprintf("%d", endpoint.SystemID)),
		uyuniLablelGroups:        model.LabelValue(strings.Join(managedGroupNames, d.separator)),
		uyuniLablelEndpointName:  model.LabelValue(endpoint.EndpointName),
		uyuniLablelExporter:      model.LabelValue(endpoint.ExporterName),
		uyuniLabelProxyModule:    model.LabelValue(endpoint.Module),
		uyuniLabelMetricsPath:    model.LabelValue(endpoint.Path),
		uyuniLabelScheme:         model.LabelValue(scheme),
	}

	return result
}

func getSystemGroupNames(systemGroupsIDs []systemGroupID) []string {
	managedGroupNames := make([]string, 0, len(systemGroupsIDs))
	for _, systemGroupInfo := range systemGroupsIDs {
		managedGroupNames = append(managedGroupNames, systemGroupInfo.GroupName)
	}

	return managedGroupNames
}

func (d *Discovery) getTargetsForSystems(
	rpcClient *xmlrpc.Client,
	token string,
	entitlement string,
) ([]model.LabelSet, error) {

	result := make([]model.LabelSet, 0)

	systemGroupIDsBySystemID, err := getSystemGroupsInfoOfMonitoredClients(rpcClient, token, entitlement)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get the managed system groups information of monitored clients")
	}

	systemIDs := make([]int, 0, len(systemGroupIDsBySystemID))
	for systemID := range systemGroupIDsBySystemID {
		systemIDs = append(systemIDs, systemID)
	}

	endpointInfos, err := getEndpointInfoForSystems(rpcClient, token, systemIDs)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get endpoints information")
	}

	networkInfoBySystemID, err := getNetworkInformationForSystems(rpcClient, token, systemIDs)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get the systems network information")
	}

	for _, endpoint := range endpointInfos {
		systemID := endpoint.SystemID
		labels := d.getEndpointLabels(
			endpoint,
			systemGroupIDsBySystemID[systemID],
			networkInfoBySystemID[systemID])
		result = append(result, labels)
	}

	return result, nil
}

func (d *Discovery) refresh(ctx context.Context) ([]*targetgroup.Group, error) {
	rpcClient, err := xmlrpc.NewClient(d.apiURL.String(), d.roundTripper)
	if err != nil {
		return nil, err
	}
	defer rpcClient.Close()

	token, err := login(rpcClient, d.username, d.password)
	if err != nil {
		return nil, errors.Wrap(err, "unable to login to Uyuni API")
	}
	defer func() {
		if err := logout(rpcClient, token); err != nil {
			level.Debug(d.logger).Log("msg", "Failed to log out from Uyuni API", "err", err)
		}
	}()

	targetsForSystems, err := d.getTargetsForSystems(rpcClient, token, d.entitlement)
	if err != nil {
		return nil, err
	}

	return []*targetgroup.Group{{Targets: targetsForSystems, Source: d.apiURL.String()}}, nil
}

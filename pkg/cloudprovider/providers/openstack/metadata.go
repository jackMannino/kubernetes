/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/utils/exec"
)

const (
	// metadataUrlTemplate allows building an OpenStack Metadata service URL.
	// It's a hardcoded IPv4 link-local address as documented in "OpenStack Cloud
	// Administrator Guide", chapter Compute - Networking with nova-network.
	//https://docs.openstack.org/nova/latest/admin/networking-nova.html#metadata-service
	defaultMetadataVersion = "2012-08-10"
	metadataUrlTemplate    = "http://169.254.169.254/openstack/%s/meta_data.json"

	// metadataID is used as an identifier on the metadata search order configuration.
	metadataID = "metadataService"

	// Config drive is defined as an iso9660 or vfat (deprecated) drive
	// with the "config-2" label.
	//https://docs.openstack.org/nova/latest/user/config-drive.html
	configDriveLabel        = "config-2"
	configDrivePathTemplate = "openstack/%s/meta_data.json"

	// configDriveID is used as an identifier on the metadata search order configuration.
	configDriveID = "configDrive"
)

var ErrBadMetadata = errors.New("invalid OpenStack metadata, got empty uuid")

// There are multiple device types. To keep it simple, we're using a single structure
// for all device metadata types.
type DeviceMetadata struct {
	Type    string `json:"type"`
	Bus     string `json:"bus,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Address string `json:"address,omitempty"`
	// .. and other fields.
}

// Assumes the "2012-08-10" meta_data.json format.
//https://docs.openstack.org/nova/latest/user/config-drive.html
type Metadata struct {
	Uuid             string           `json:"uuid"`
	Hostname         string           `json:"hostname"`
	AvailabilityZone string           `json:"availability_zone"`
	Devices          []DeviceMetadata `json:"devices,omitempty"`
	// .. and other fields we don't care about.  Expand as necessary.
}

// parseMetadata reads JSON from OpenStack metadata server and parses
// instance ID out of it.
func parseMetadata(r io.Reader) (*Metadata, error) {
	var metadata Metadata
	json := json.NewDecoder(r)
	if err := json.Decode(&metadata); err != nil {
		return nil, err
	}

	if metadata.Uuid == "" {
		return nil, ErrBadMetadata
	}

	return &metadata, nil
}

func getMetadataUrl(metadataVersion string) string {
	return fmt.Sprintf(metadataUrlTemplate, metadataVersion)
}

func getConfigDrivePath(metadataVersion string) string {
	return fmt.Sprintf(configDrivePathTemplate, metadataVersion)
}

func getMetadataFromConfigDrive(metadataVersion string) (*Metadata, error) {
	// Try to read instance UUID from config drive.
	dev := "/dev/disk/by-label/" + configDriveLabel
	if _, err := os.Stat(dev); os.IsNotExist(err) {
		out, err := exec.New().Command(
			"blkid", "-l",
			"-t", "LABEL="+configDriveLabel,
			"-o", "device",
		).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("unable to run blkid: %v", err)
		}
		dev = strings.TrimSpace(string(out))
	}

	mntdir, err := ioutil.TempDir("", "configdrive")
	if err != nil {
		return nil, err
	}
	defer os.Remove(mntdir)

	glog.V(4).Infof("Attempting to mount configdrive %s on %s", dev, mntdir)

	mounter := mount.New("" /* default mount path */)
	err = mounter.Mount(dev, mntdir, "iso9660", []string{"ro"})
	if err != nil {
		err = mounter.Mount(dev, mntdir, "vfat", []string{"ro"})
	}
	if err != nil {
		return nil, fmt.Errorf("error mounting configdrive %s: %v", dev, err)
	}
	defer mounter.Unmount(mntdir)

	glog.V(4).Infof("Configdrive mounted on %s", mntdir)

	configDrivePath := getConfigDrivePath(metadataVersion)
	f, err := os.Open(
		filepath.Join(mntdir, configDrivePath))
	if err != nil {
		return nil, fmt.Errorf("error reading %s on config drive: %v", configDrivePath, err)
	}
	defer f.Close()

	return parseMetadata(f)
}

func getMetadataFromMetadataService(metadataVersion string) (*Metadata, error) {
	// Try to get JSON from metadata server.
	metadataUrl := getMetadataUrl(metadataVersion)
	glog.V(4).Infof("Attempting to fetch metadata from %s", metadataUrl)
	resp, err := http.Get(metadataUrl)
	if err != nil {
		return nil, fmt.Errorf("error fetching %s: %v", metadataUrl, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("unexpected status code when reading metadata from %s: %s", metadataUrl, resp.Status)
		return nil, err
	}

	return parseMetadata(resp.Body)
}

// Metadata is fixed for the current host, so cache the value process-wide
var metadataCache *Metadata

func getMetadata(order string) (*Metadata, error) {
	if metadataCache == nil {
		var md *Metadata
		var err error

		elements := strings.Split(order, ",")
		for _, id := range elements {
			id = strings.TrimSpace(id)
			switch id {
			case configDriveID:
				md, err = getMetadataFromConfigDrive(defaultMetadataVersion)
			case metadataID:
				md, err = getMetadataFromMetadataService(defaultMetadataVersion)
			default:
				err = fmt.Errorf("%s is not a valid metadata search order option. Supported options are %s and %s", id, configDriveID, metadataID)
			}

			if err == nil {
				break
			}
		}

		if err != nil {
			return nil, err
		}
		metadataCache = md
	}
	return metadataCache, nil
}

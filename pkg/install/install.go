// Copyright (c) 2020 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package install

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/howeyc/fsnotify"
	"github.com/kelseyhightower/envconfig"
	cp "github.com/nmrshll/go-cp"
	"github.com/projectcalico/libcalico-go/lib/names"
	"github.com/prometheus/common/log"
	"github.com/sirupsen/logrus"
	"go.etcd.io/etcd/pkg/fileutil"
	"k8s.io/client-go/rest"
)

type config struct {
	// Location on the host where CNI network configs are stored.
	CNINetDir   string `envconfig:"CNI_NET_DIR" default:"/etc/cni/net.d"`
	CNIConfName string `envconfig:"CNI_CONF_NAME"`

	// Directory where we expect that TLS assets will be mounted into the calico/cni container.
	TLSAssetsDir string `envconfig:"TLS_ASSETS_DIR" default:"/calico-secrets"`

	// SkipCNIBinaries is a comma-separated list of binaries. Each binary in the list
	// will be skipped when installing to the host.
	SkipCNIBinaries []string `envconfig:"SKIP_CNI_BINARIES"`

	// UpdateCNIBinaries controls whether or not to overwrite any binaries with the same name
	// on the host.
	UpdateCNIBinaries bool `envconfig:"UPDATE_CNI_BINARIES"`

	// The CNI network configuration to install.
	CNINetworkConfig     string `envconfig:"CNI_NETWORK_CONFIG"`
	CNINetworkConfigFile string `envconfig:"CNI_NETWORK_CONFIG_FILE"`

	ShouldSleep bool `envconfig:"SLEEP" default:"true"`

	ServiceAccountToken []byte
}

func (c config) skipBinary(binary string) bool {
	for _, name := range c.SkipCNIBinaries {
		if name == binary {
			return true
		}
	}
	return false
}

func getEnv(env, def string) string {
	if val, ok := os.LookupEnv(env); ok {
		return val
	}
	return def
}

func directoryExists(dir string) bool {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		logrus.WithError(err).Fatalf("Failed to check if directory %s exists", dir)
		return false
	}
	return info.IsDir()
}

func fileExists(file string) bool {
	info, err := os.Stat(file)
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		logrus.WithError(err).Fatalf("Failed to check if file %s exists", file)
		return false
	}
	return !info.IsDir()
}

func mkdir(path string) {
	if err := os.MkdirAll(path, 0777); err != nil {
		logrus.WithError(err).Fatalf("Failed to create directory %s", path)
	}

}

func loadConfig() config {
	var c config
	err := envconfig.Process("", &c)
	if err != nil {
		logrus.Fatal(err.Error())
	}

	return c
}

func Install() error {
	// Clean up any existing binaries / config / assets.
	if err := os.Remove("/host/opt/cni/bin/calico"); err != nil && !os.IsNotExist(err) {
		logrus.WithError(err).Warnf("Error removing old plugin")
	}
	if err := os.Remove("/host/opt/cni/bin/calico-ipam"); err != nil && !os.IsNotExist(err) {
		logrus.WithError(err).Warnf("Error removing old IPAM plugin")
	}
	if err := os.RemoveAll("/host/etc/cni/net.d/calico-tls"); err != nil && !os.IsNotExist(err) {
		logrus.WithError(err).Warnf("Error removing old TLS directory")
	}

	// Load config.
	c := loadConfig()

	// Determine if we're running as a Kubernetes pod.
	var kubecfg *rest.Config

	serviceAccountTokenFile := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	c.ServiceAccountToken = make([]byte, 0)
	var err error
	if fileExists(serviceAccountTokenFile) {
		log.Info("Running as a Kubernetes pod")
		kubecfg, err = rest.InClusterConfig()
		if err != nil {
			return err
		}
		err = rest.LoadTLSFiles(kubecfg)
		if err != nil {
			return err
		}

		c.ServiceAccountToken, err = ioutil.ReadFile(serviceAccountTokenFile)
		if err != nil {
			return err
		}
	}

	// Copy over any TLS assets from the SECRETS_MOUNT_DIR to the host.
	// First check if the dir exists and has anything in it.
	if directoryExists(c.TLSAssetsDir) {
		logrus.Info("Installing any TLS assets")
		mkdir("/host/etc/cni/net.d/calico-tls")
		if err := copyFileAndPermissions(fmt.Sprintf("%s/%s", c.TLSAssetsDir, "etcd-ca"), "/host/etc/cni/net.d/calico-tls/etcd-ca"); err != nil {
			logrus.Warnf("Missing etcd-ca")
		}
		if err := copyFileAndPermissions(fmt.Sprintf("%s/%s", c.TLSAssetsDir, "etcd-cert"), "/host/etc/cni/net.d/calico-tls/etcd-cert"); err != nil {
			logrus.Warnf("Missing etcd-cert")
		}
		if err := copyFileAndPermissions(fmt.Sprintf("%s/%s", c.TLSAssetsDir, "etcd-key"), "/host/etc/cni/net.d/calico-tls/etcd-key"); err != nil {
			logrus.Warnf("Missing etcd-key")
		}
	}

	// Copy install to calico and calico-ipam
	if err := copyFileAndPermissions("/opt/cni/bin/install", "/opt/cni/bin/calico"); err != nil {
		logrus.WithError(err).Fatalf("Failed to copy install to calico")
	}
	if err := copyFileAndPermissions("/opt/cni/bin/install", "/opt/cni/bin/calico-ipam"); err != nil {
		logrus.WithError(err).Fatalf("Failed to copy install to calico-ipam")
	}

	// Place the new binaries if the directory is writeable.
	dirs := []string{"/host/opt/cni/bin", "/host/secondary-bin-dir"}
	for _, d := range dirs {
		if err := fileutil.IsDirWriteable(d); err != nil {
			logrus.Infof("%s is not writeable, skipping", d)
			continue
		}

		// Iterate through each binary we might want to install.
		files, err := ioutil.ReadDir("/opt/cni/bin/")
		if err != nil {
			log.Fatal(err)
		}
		for _, binary := range files {
			target := fmt.Sprintf("%s/%s", d, binary.Name())
			source := fmt.Sprintf("/opt/cni/bin/%s", binary.Name())
			if strings.Contains(binary.Name(), "calico") {
				// For Calico binaries, we copy over the install binary. It includes the code
				// for each, and the name of the binary determined which is executed.
				source = "/opt/cni/bin/install"
			}
			if c.skipBinary(binary.Name()) {
				continue
			}
			if fileExists(target) && c.UpdateCNIBinaries {
				logrus.Infof("Skipping installation of %s", target)
				continue
			}
			if err := copyFileAndPermissions(source, target); err != nil {
				logrus.WithError(err).Errorf("Failed to install %s", target)
				os.Exit(1)
			}
			logrus.Infof("Installed %s", target)
		}

		logrus.Infof("Wrote Calico CNI binaries to %s\n", d)

		// Print CNI plugin version
		cmd := exec.Command(d+"/calico", "-v")
		var out bytes.Buffer
		cmd.Stdout = &out
		err = cmd.Run()
		if err != nil {
			logrus.WithError(err).Warnf("Failed getting CNI plugin version")
		}
		logrus.Infof("CNI plugin version: %s", out.String())
	}

	if kubecfg != nil {
		// If running as a Kubernetes pod, then write out a kubeconfig for the
		// CNI plugin to use.
		writeKubeconfig(kubecfg)
	}

	// Write a CNI config file.
	writeCNIConfig(c)

	// Unless told otherwise, sleep forever.
	// This prevents Kubernetes from restarting the pod repeatedly.
	logrus.Infof("Done configuring CNI.  Sleep= %v", c.ShouldSleep)
	for c.ShouldSleep {
		// Kubernetes Secrets can be updated.  If so, we need to install the updated
		// version to the host. Just check the timestamp on the certificate to see if it
		// has been updated.  A bit hokey, but likely good enough.
		filename := c.TLSAssetsDir + "/etcd-cert"

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}

		done := make(chan bool)

		// Process events
		go func() {
			for {
				select {
				case <-watcher.Event:
					logrus.Infoln("Updating installed secrets at:", time.Now().String())
					files, err := ioutil.ReadDir(c.TLSAssetsDir)
					if err != nil {
						logrus.Warn(err)
					}
					for _, f := range files {
						if err = copyFileAndPermissions(c.TLSAssetsDir+"/"+f.Name(), "/host/etc/cni/net.d/calico-tls/"+f.Name()); err != nil {
							logrus.Warn(err)
							continue
						}
					}
				case err := <-watcher.Error:
					log.Fatal(err)
				}
			}
		}()

		err = watcher.Watch(filename)
		if err != nil {
			log.Fatal(err)
		}

		<-done

		watcher.Close()
	}
	return nil
}

func writeCNIConfig(c config) {
	netconf := `{
  "name": "k8s-pod-network",
  "cniVersion": "0.3.1", 
  "plugins": [
    {
      "type": "calico",
      "log_level": "__LOG_LEVEL__",
      "datastore_type": "__DATASTORE_TYPE__",
      "nodename": "__KUBERNETES_NODE_NAME__",
      "mtu": __CNI_MTU__,
      "ipam": {"type": "calico-ipam"},
      "policy": {"type": "k8s"},
      "kubernetes": {"kubeconfig": "__KUBECONFIG_FILEPATH__"}
    },
    {
      "type": "portmap",
      "snat": true,
      "capabilities": {"portMappings": true}
    }
  ],
}`

	// Pick the config template to use. This can either be through an env var,
	// or a file mounted into the container.
	if c.CNINetworkConfig != "" {
		log.Info("Using CNI config template from CNI_NETWORK_CONFIG environment variable.")
		netconf = c.CNINetworkConfig
	}
	if c.CNINetworkConfigFile != "" {
		log.Info("Using CNI config template from CNI_NETWORK_CONFIG_FILE")
		var err error
		netconfBytes, err := ioutil.ReadFile(c.CNINetworkConfigFile)
		if err != nil {
			log.Fatal(err)
		}
		netconf = string(netconfBytes)
	}

	kubeconfigPath := c.CNINetDir + "/calico-kubeconfig"

	// Perform replacements of variables.
	nodename, err := names.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	netconf = strings.Replace(netconf, "__LOG_LEVEL__", getEnv("LOG_LEVEL", "info"), -1)
	netconf = strings.Replace(netconf, "__LOG_FILE_PATH__", getEnv("LOG_FILE_PATH", "/var/log/calico/cni/cni.log"), -1)
	netconf = strings.Replace(netconf, "__DATASTORE_TYPE__", getEnv("DATASTORE_TYPE", "kubernetes"), -1)
	netconf = strings.Replace(netconf, "__KUBERNETES_NODE_NAME__", getEnv("NODENAME", nodename), -1)
	netconf = strings.Replace(netconf, "__KUBECONFIG_FILEPATH__", kubeconfigPath, -1)
	netconf = strings.Replace(netconf, "__CNI_MTU__", getEnv("CNI_MTU", "1500"), -1)

	netconf = strings.Replace(netconf, "__KUBERNETES_SERVICE_HOST__", getEnv("KUBERNETES_SERVICE_HOST", ""), -1)
	netconf = strings.Replace(netconf, "__KUBERNETES_SERVICE_PORT__", getEnv("KUBERNETES_SERVICE_PORT", ""), -1)

	netconf = strings.Replace(netconf, "__SERVICEACCOUNT_TOKEN__", string(c.ServiceAccountToken), -1)

	// Replace etcd datastore variables.
	hostSecretsDir := c.CNINetDir + "/calico-tls"

	etcdCertFile := fmt.Sprintf("%s/etcd-cert", hostSecretsDir)
	if fileExists(etcdCertFile) {
		netconf = strings.Replace(netconf, "__ETCD_CERT_FILE__", etcdCertFile, -1)
	} else {
		netconf = strings.Replace(netconf, "__ETCD_CERT_FILE__", "", -1)
	}

	etcdCACertFile := fmt.Sprintf("%s/etcd-ca", hostSecretsDir)
	if fileExists(etcdCACertFile) {
		netconf = strings.Replace(netconf, "__ETCD_CA_CERT_FILE__", etcdCACertFile, -1)
	} else {
		netconf = strings.Replace(netconf, "__ETCD_CA_CERT_FILE__", "", -1)
	}

	etcdKeyFile := fmt.Sprintf("%s/etcd-key", hostSecretsDir)
	if fileExists(etcdKeyFile) {
		netconf = strings.Replace(netconf, "__ETCD_KEY_FILE__", etcdKeyFile, -1)
	} else {
		netconf = strings.Replace(netconf, "__ETCD_KEY_FILE__", "", -1)
	}
	netconf = strings.Replace(netconf, "__ETCD_ENDPOINTS__", getEnv("ETCD_ENDPOINTS", ""), -1)
	netconf = strings.Replace(netconf, "__ETCD_DISCOVERY_SRV__", getEnv("ETCD_DISCOVERY_SRV", ""), -1)

	// Write out the file.
	name := getEnv("CNI_CONF_NAME", "10-calico.conflist")
	path := fmt.Sprintf("/host/etc/cni/net.d/%s", name)
	err = ioutil.WriteFile(path, []byte(netconf), 0644)
	if err != nil {
		log.Fatal(err)
	}

	content, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	logrus.Infof("Created /host/etc/cni/net.d/%s", name)
	text := string(content)
	fmt.Println(text)

	// Remove any old config file, if one exists.
	oldName := getEnv("CNI_OLD_CONF_NAME", "10-calico.conflist")
	if name != oldName {
		logrus.Infof("Removing /host/etcd/cni/net.d/%s", oldName)
		if err := os.Remove(fmt.Sprintf("/host/etcd/cni/net.d/%s", oldName)); err != nil {
			logrus.WithError(err).Warnf("Failed to remove %s", oldName)
		}
	}
}

// copyFileAndPermissions copies file permission
func copyFileAndPermissions(src, dst string) (err error) {
	if err := cp.CopyFile(src, dst); err != nil {
		return err
	}

	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	err = os.Chmod(dst, si.Mode())
	if err != nil {
		return err
	}

	return
}

func writeKubeconfig(kubecfg *rest.Config) {
	data := `# Kubeconfig file for Calico CNI plugin.
apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: __KUBERNETES_SERVICE_PROTOCOL__://[__KUBERNETES_SERVICE_HOST__]:__KUBERNETES_SERVICE_PORT__
    __TLS_CFG__
users:
- name: calico
  user:
    token: TOKEN
contexts:
- name: calico-context
  context:
    cluster: local
    user: calico
current-context: calico-context`

	data = strings.Replace(data, "TOKEN", kubecfg.BearerToken, 1)
	data = strings.Replace(data, "__KUBERNETES_SERVICE_PROTOCOL__", getEnv("KUBERNETES_SERVICE_PROTOCOL", "https"), -1)
	data = strings.Replace(data, "__KUBERNETES_SERVICE_HOST__", getEnv("KUBERNETES_SERVICE_HOST", ""), -1)
	data = strings.Replace(data, "__KUBERNETES_SERVICE_PORT__", getEnv("KUBERNETES_SERVICE_PORT", ""), -1)

	skipTLSVerify := os.Getenv("SKIP_TLS_VERIFY")
	if skipTLSVerify == "true" {
		data = strings.Replace(data, "__TLS_CFG__", "insecure-skip-tls-verify: true", -1)
	} else {
		ca := "certificate-authority-data: " + base64.StdEncoding.EncodeToString(kubecfg.CAData)
		data = strings.Replace(data, "__TLS_CFG__", ca, -1)
	}

	if err := ioutil.WriteFile("/host/etc/cni/net.d/calico-kubeconfig", []byte(data), 0600); err != nil {
		log.Fatal(err)
	}
}

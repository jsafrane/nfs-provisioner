/*
Copyright 2016 Red Hat, Inc.

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

package volume

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/golang/glog"
	"github.com/guelfey/go.dbus"
	"github.com/wongma7/nfs-provisioner/controller"
	"k8s.io/client-go/1.4/dynamic"
	"k8s.io/client-go/1.4/kubernetes"
	"k8s.io/client-go/1.4/pkg/api/unversioned"
	"k8s.io/client-go/1.4/pkg/api/v1"
	"k8s.io/client-go/1.4/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/1.4/pkg/runtime"
)

const (
	// ValidatedPSPAnnotation is the annotation on the pod object that specifies
	// the name of the PSP the pod validated against, if any.
	ValidatedPSPAnnotation = "kubernetes.io/psp"

	// ValidatedSCCAnnotation is the annotation on the pod object that specifies
	// the name of the SCC the pod validated against, if any.
	ValidatedSCCAnnotation = "openshift.io/scc"

	// VolumeGidAnnotationKey is the key of the annotation on the PersistentVolume
	// object that specifies a supplemental GID.
	VolumeGidAnnotationKey = "pv.beta.kubernetes.io/gid"

	// A PV annotation for the entire ganesha EXPORT block, needed for ganesha
	// deletion.
	annBlock = "EXPORT_block"

	// A PV annotation for the Export_Id of this PV's backing ganesha EXPORT,
	// needed for ganesha deletion.
	annExportId = "Export_Id"

	// A PV annotation for the line in /etc/exports, needed for kernel deletion.
	annLine = "etcexports_line"

	// are we allowed to set this? else make up our own
	annCreatedBy = "kubernetes.io/createdby"
	createdBy    = "nfs-dynamic-provisioner"
)

func NewNFSProvisioner(exportDir string, client kubernetes.Interface, dynamicClient *dynamic.Client, useGanesha bool, ganeshaConfig string) controller.Provisioner {
	provisioner := &nfsProvisioner{
		exportDir:     exportDir,
		client:        client,
		useGanesha:    useGanesha,
		ganeshaConfig: ganeshaConfig,
		nextExportId:  0,
		mutex:         &sync.Mutex{},
		podIPEnv:      "MY_POD_IP",
		serviceEnv:    "MY_SERVICE_NAME",
		namespaceEnv:  "MY_POD_NAMESPACE",
	}
	provisioner.ranges = getSupplementalGroupsRanges(client, dynamicClient, "/podinfo/annotations", os.Getenv(provisioner.namespaceEnv))

	return provisioner
}

type nfsProvisioner struct {
	// The directory to create PV-backing directories in
	exportDir string

	// Client, needed for getting a service cluster IP to put as the NFS server of
	// provisioned PVs
	client kubernetes.Interface

	// Whether to use NFS Ganesha (D-Bus method calls) or kernel NFS server
	// (exportfs)
	useGanesha bool

	// The path of the NFS Ganesha configuration file
	ganeshaConfig string

	// Incremented for assigning each export a unique ID, required by ganesha
	nextExportId int

	// Lock for writing to the ganesha config or /etc/exports file
	mutex *sync.Mutex

	// Ranges of gids to assign to PV's
	ranges []v1beta1.IDRange

	// Environment variables the provisioner pod needs valid values for in order to
	// put a service cluster IP as the server of provisioned NFS PVs, passed in
	// via downward API. If serviceEnv is set, namespaceEnv must be too.
	podIPEnv     string
	serviceEnv   string
	namespaceEnv string
}

var _ controller.Provisioner = &nfsProvisioner{}

// Provision creates a volume i.e. the storage asset and returns a PV object for
// the volume.
func (p *nfsProvisioner) Provision(options controller.VolumeOptions) (*v1.PersistentVolume, error) {
	server, path, gid, added, exportId, err := p.createVolume(options)
	if err != nil {
		return nil, err
	}

	annotations := make(map[string]string)
	annotations[annCreatedBy] = createdBy
	annotations[VolumeGidAnnotationKey] = strconv.FormatInt(gid, 10)
	if p.useGanesha {
		annotations[annBlock] = added
		annotations[annExportId] = strconv.Itoa(exportId)
	} else {
		annotations[annLine] = added
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: v1.ObjectMeta{
			Name:        options.PVName,
			Labels:      map[string]string{},
			Annotations: annotations,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: options.PersistentVolumeReclaimPolicy,
			AccessModes:                   options.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.Capacity,
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				NFS: &v1.NFSVolumeSource{
					Server:   server,
					Path:     path,
					ReadOnly: false,
				},
			},
		},
	}

	return pv, nil
}

// createVolume creates a volume i.e. the storage asset. It creates a unique
// directory under /export and exports it. Returns the server IP, the path, and
// gid. Also returns the block or line it added to either the ganesha config or
// /etc/exports, respectively. If using ganesha, returns a non-zero Export_Id.
func (p *nfsProvisioner) createVolume(options controller.VolumeOptions) (string, string, int64, string, int, error) {
	// TODO take and validate Parameters
	if options.Parameters != nil {
		return "", "", 0, "", 0, fmt.Errorf("invalid parameter: no StorageClass parameters are supported")
	}

	// TODO implement options.ProvisionerSelector parsing
	// TODO pv.Labels MUST be set to match claim.spec.selector
	if options.Selector != nil {
		return "", "", 0, "", 0, fmt.Errorf("claim.Spec.Selector is not supported")
	}

	server, err := p.getServer()
	if err != nil {
		return "", "", 0, "", 0, fmt.Errorf("error getting NFS server IP for created volume: %v", err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(p.exportDir, &stat); err != nil {
		return "", "", 0, "", 0, fmt.Errorf("error calling statfs on %v: %v", p.exportDir, err)
	}
	capacity := options.Capacity.Value()
	// Available blocks * size per block = available space in bytes
	available := int64(stat.Bavail) * stat.Bsize
	if capacity > available {
		return "", "", 0, "", 0, fmt.Errorf("not enough available space %v bytes to satisfy claim for %v bytes", available, capacity)
	}

	// TODO quota, something better than just directories
	// TODO figure out permissions: gid, chgrp, root_squash
	// Create the path for the volume unless it already exists. It has to exist
	// when AddExport or exportfs is called.
	path := fmt.Sprintf(p.exportDir+"%s", options.PVName)
	if _, err := os.Stat(path); err == nil {
		return "", "", 0, "", 0, fmt.Errorf("error creating volume, the path already exists")
	}
	// Execute permission is required for stat, which kubelet uses during unmount.
	if err := os.MkdirAll(path, 0071); err != nil {
		return "", "", 0, "", 0, fmt.Errorf("error creating dir for volume: %v", err)
	}
	// Due to umask, need to chmod
	cmd := exec.Command("chmod", "071", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(path)
		return "", "", 0, "", 0, fmt.Errorf("chmod failed with error: %v, output: %s", err, out)
	}

	gid, err := p.generateSupplementalGroup()
	if err != nil {
		return "", "", 0, "", 0, fmt.Errorf("error generating SupplementalGroup: %v", err)
	}
	cmd = exec.Command("chgrp", strconv.FormatInt(gid, 10), path)
	out, err = cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(path)
		return "", "", 0, "", 0, fmt.Errorf("chgrp failed with error: %v, output: %s", err, out)
	}

	if p.useGanesha {
		block, exportId, err := p.ganeshaExport(path)
		if err != nil {
			os.RemoveAll(path)
			return "", "", 0, "", 0, err
		}
		return server, path, gid, block, exportId, nil
	} else {
		line, err := p.kernelExport(path)
		if err != nil {
			os.RemoveAll(path)
			return "", "", 0, "", 0, err
		}
		return server, path, gid, line, 0, nil
	}
}

// getServer gets the server IP to put in a provisioned PV's spec.
func (p *nfsProvisioner) getServer() (string, error) {
	// Use either `hostname -i` or podIPEnv as the fallback server
	var fallbackServer string
	podIP := os.Getenv(p.podIPEnv)
	if podIP == "" {
		glog.Infof("pod IP env %s isn't set or provisioner isn't running as a pod", p.podIPEnv)
		out, err := exec.Command("hostname", "-i").Output()
		if err != nil {
			return "", fmt.Errorf("hostname -i failed with error: %v, output: %s", err, out)
		}
		fallbackServer = string(out)
	} else {
		fallbackServer = podIP
	}

	// Try to use the service's cluster IP as the server if serviceEnv is
	// specified. Otherwise, use fallback here.
	serviceName := os.Getenv(p.serviceEnv)
	if serviceName == "" {
		glog.Infof("service env %s isn't set, falling back to using `hostname -i` or pod IP as server IP", p.serviceEnv)
		return fallbackServer, nil
	}

	// From this point forward, rather than fallback & provision non-persistent
	// where persistent is expected, just return an error.
	namespace := os.Getenv(p.namespaceEnv)
	if namespace == "" {
		return "", fmt.Errorf("service env %s is set but namespace env %s isn't; no way to get the service cluster IP", p.serviceEnv, p.namespaceEnv)
	}
	service, err := p.client.Core().Services(namespace).Get(serviceName)
	if err != nil {
		return "", fmt.Errorf("error getting service %s=%s in namespace %s=%s", p.serviceEnv, serviceName, p.namespaceEnv, namespace)
	}

	// Do some validation of the service before provisioning useless volumes
	valid := false
	type endpointPort struct {
		port     int32
		protocol v1.Protocol
	}
	expectedPorts := map[endpointPort]bool{
		endpointPort{2049, v1.ProtocolTCP}:  true,
		endpointPort{20048, v1.ProtocolTCP}: true,
		endpointPort{111, v1.ProtocolUDP}:   true,
		endpointPort{111, v1.ProtocolTCP}:   true,
	}
	endpoints, err := p.client.Core().Endpoints(namespace).Get(serviceName)
	for _, subset := range endpoints.Subsets {
		if len(subset.Addresses) != 1 {
			continue
		}
		if subset.Addresses[0].IP != fallbackServer {
			continue
		}
		actualPorts := make(map[endpointPort]bool)
		for _, port := range subset.Ports {
			actualPorts[endpointPort{port.Port, port.Protocol}] = true
		}
		if !reflect.DeepEqual(expectedPorts, actualPorts) {
			continue
		}
		valid = true
		break
	}
	if !valid {
		return "", fmt.Errorf("service %s=%s is not valid; check that it has for ports %v one endpoint, this pod's IP %v", p.serviceEnv, serviceName, expectedPorts, fallbackServer)
	}
	if service.Spec.ClusterIP == v1.ClusterIPNone {
		return "", fmt.Errorf("service %s=%s is valid but it doesn't have a cluster IP", p.serviceEnv, serviceName)
	}

	return service.Spec.ClusterIP, nil
}

// generateSupplementalGroup generates a random SupplementalGroup from the
// provisioners ranges of SupplementalGroups. Picks a random range then a random
// value within it
// TODO make this better
func (p *nfsProvisioner) generateSupplementalGroup() (int64, error) {
	if len(p.ranges) == 0 {
		return 0, fmt.Errorf("provisioner has empty ranges, can't generate SupplementalGroup")
	}
	rng := p.ranges[0]
	if len(p.ranges) > 0 {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(p.ranges))))
		if err != nil {
			return 0, fmt.Errorf("error getting rand value: %v", err)
		}
		rng = p.ranges[i.Int64()]
	}
	i, err := rand.Int(rand.Reader, big.NewInt(rng.Max-rng.Min+1))
	if err != nil {
		return 0, fmt.Errorf("error getting rand value: %v", err)
	}
	return rng.Min + i.Int64(), nil
}

// ganeshaExport exports the given directory using NFS Ganesha, assuming it is
// running and can be connected to using D-Bus. Returns the block it added to
// the ganesha config file and the block's Export_Id.
// https://github.com/nfs-ganesha/nfs-ganesha/wiki/Dbusinterface
func (p *nfsProvisioner) ganeshaExport(path string) (string, int, error) {
	// Create the export block to add to the ganesha config file
	p.mutex.Lock()
	read, err := ioutil.ReadFile(p.ganeshaConfig)
	if err != nil {
		p.mutex.Unlock()
		return "", 0, err
	}
	// TODO there's probably a better way to do this. HAVE to assign unique IDs
	// across restarts, etc.
	// If zero, this is the first add: find the maximum existing ID and the next
	// ID to assign will be that maximum plus 1. Otherwise just keep incrementing.
	if p.nextExportId == 0 {
		re := regexp.MustCompile("Export_Id = [0-9]+;")
		lines := re.FindAll(read, -1)
		for _, line := range lines {
			digits := regexp.MustCompile("[0-9]+").Find(line)
			if id, _ := strconv.Atoi(string(digits)); id > p.nextExportId {
				p.nextExportId = id
			}
		}
	}
	p.nextExportId++
	exportId := p.nextExportId
	p.mutex.Unlock()

	block := "\nEXPORT\n{\n"
	block = block + "\tExport_Id = " + strconv.Itoa(exportId) + ";\n"
	block = block + "\tPath = " + path + ";\n" +
		"\tPseudo = " + path + ";\n" +
		"\tAccess_Type = RW;\n" +
		"\tSquash = root_id_squash;\n" +
		"\tSecType = sys;\n" +
		"\tFilesystem_id = " + strconv.Itoa(exportId) + "." + strconv.Itoa(exportId) + ";\n" +
		"\tFSAL {\n\t\tName = VFS;\n\t}\n}\n"

	// Add the export block to the ganesha config file
	if err := p.addToFile(p.ganeshaConfig, block); err != nil {
		return "", 0, fmt.Errorf("error adding export block to the ganesha config file: %v", err)
	}

	// Call AddExport using dbus
	conn, err := dbus.SystemBus()
	if err != nil {
		p.removeFromFile(p.ganeshaConfig, block)
		return "", 0, fmt.Errorf("error getting dbus session bus: %v", err)
	}
	obj := conn.Object("org.ganesha.nfsd", "/org/ganesha/nfsd/ExportMgr")
	call := obj.Call("org.ganesha.nfsd.exportmgr.AddExport", 0, p.ganeshaConfig, fmt.Sprintf("export(path = %s)", path))
	if call.Err != nil {
		p.removeFromFile(p.ganeshaConfig, block)
		return "", 0, fmt.Errorf("error calling org.ganesha.nfsd.exportmgr.AddExport: %v", call.Err)
	}

	return block, exportId, nil
}

// kernelExport exports the given directory using the NFS server, assuming it is
// running. Returns the line it added to /etc/exports.
func (p *nfsProvisioner) kernelExport(path string) (string, error) {
	line := "\n" + path + " *(rw,insecure,root_squash)\n"

	// Add the export directory line to /etc/exports
	if err := p.addToFile("/etc/exports", line); err != nil {
		return "", fmt.Errorf("error adding export directory to /etc/exports: %v", err)
	}

	// Execute exportfs
	cmd := exec.Command("exportfs", "-r")
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.removeFromFile("/etc/exports", line)
		return "", fmt.Errorf("exportfs -r failed with error: %v, output: %s", err, out)
	}

	return line, nil
}

func (p *nfsProvisioner) addToFile(path string, toAdd string) error {
	p.mutex.Lock()

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		p.mutex.Unlock()
		return err
	}
	defer file.Close()

	if _, err = file.WriteString(toAdd); err != nil {
		p.mutex.Unlock()
		return err
	}
	file.Sync()

	p.mutex.Unlock()
	return nil
}

func (p *nfsProvisioner) removeFromFile(path string, toRemove string) error {
	p.mutex.Lock()

	read, err := ioutil.ReadFile(path)
	if err != nil {
		p.mutex.Unlock()
		return err
	}

	removed := strings.Replace(string(read), toRemove, "", -1)
	err = ioutil.WriteFile(path, []byte(removed), 0)
	if err != nil {
		p.mutex.Unlock()
		return err
	}

	p.mutex.Unlock()
	return nil
}

// getSupplementalGroupsRanges gets the ranges of SupplementalGroups the
// provisioner pod is allowed to run as. Range rules can be imposed by a PSP,
// SCC or namespace (latter two in openshift only).
func getSupplementalGroupsRanges(client kubernetes.Interface, dynamicClient *dynamic.Client, downwardAnnotationsFile string, namespace string) []v1beta1.IDRange {
	sccName, err := getPodAnnotation(downwardAnnotationsFile, ValidatedSCCAnnotation)
	if err != nil {
		glog.Errorf("error getting pod annotation %s: %v", ValidatedSCCAnnotation, err)
	} else if sccName != "" {
		sccResource := unversioned.APIResource{Name: "securitycontextconstraints", Namespaced: false, Kind: "SecurityContextConstraints"}
		sccClient := dynamicClient.Resource(&sccResource, "")
		scc, err := sccClient.Get(sccName)
		if err != nil {
			glog.Errorf("error getting provisioner pod's scc: %v", err)
		} else {
			ranges := getSCCSupplementalGroups(scc)
			if ranges != nil && len(ranges) > 0 {
				return ranges
			}
		}
	}

	pspName, err := getPodAnnotation(downwardAnnotationsFile, ValidatedPSPAnnotation)
	if err != nil {
		glog.Errorf("error getting pod annotation %s: %v", ValidatedPSPAnnotation, err)
	} else if pspName != "" {
		psp, err := client.Extensions().PodSecurityPolicies().Get(pspName)
		if err != nil {
			glog.Errorf("error getting provisioner pod's psp: %v", err)
		} else {
			ranges := getPSPSupplementalGroups(psp)
			if ranges != nil && len(ranges) > 0 {
				return ranges
			}
		}
	}

	ns, err := client.Core().Namespaces().Get(namespace)
	if err != nil {
		glog.Errorf("error getting namespace %s: %v", namespace, err)
	} else {
		ranges, err := getPreallocatedSupplementalGroups(ns)
		if err != nil {
			glog.Errorf("error getting preallcoated supplemental groups: %v", err)
		} else if ranges != nil && len(ranges) > 0 {
			return ranges
		}
	}

	return []v1beta1.IDRange{{Min: int64(0), Max: int64(65533)}}
}

// getPodAnnotation returns the value of the given annotation on the provisioner
// pod or an empty string if the annotation doesn't exist.
func getPodAnnotation(downwardAnnotationsFile string, annotation string) (string, error) {
	read, err := ioutil.ReadFile(downwardAnnotationsFile)
	if err != nil {
		return "", fmt.Errorf("error reading downward API annotations volume: %v", err)
	}
	re := regexp.MustCompile(annotation + "=\".*\"$")
	line := re.Find(read)
	if line == nil {
		return "", nil
	}
	re = regexp.MustCompile("\"(.*?)\"")
	value := re.FindStringSubmatch(string(line))[1]
	return value, nil
}

// getPSPSupplementalGroups returns the SupplementalGroup Ranges of the PSP or
// nil if the PSP doesn't impose gid range rules.
func getPSPSupplementalGroups(psp *v1beta1.PodSecurityPolicy) []v1beta1.IDRange {
	if psp == nil {
		return nil
	}
	if psp.Spec.SupplementalGroups.Rule != v1beta1.SupplementalGroupsStrategyMustRunAs {
		return nil
	}
	return psp.Spec.SupplementalGroups.Ranges
}

// TODO "Type" vs "Rule"
// SupplementalGroupsStrategyOptions defines the strategy type and options used to create the strategy.
type SupplementalGroupsStrategyOptions struct {
	// Type is the strategy that will dictate what supplemental groups is used in the SecurityContext.
	Type v1beta1.SupplementalGroupsStrategyType `json:"type,omitempty" protobuf:"bytes,1,opt,name=type,casttype=SupplementalGroupsStrategyType"`
	// Ranges are the allowed ranges of supplemental groups.  If you would like to force a single
	// supplemental group then supply a single range with the same start and end.
	Ranges []v1beta1.IDRange `json:"ranges,omitempty" protobuf:"bytes,2,rep,name=ranges"`
}

// getSCCSupplementalGroups returns the SupplementalGroup Ranges of the SCC or
// nil if the SCC doesn't impose gid range rules.
func getSCCSupplementalGroups(scc *runtime.Unstructured) []v1beta1.IDRange {
	if scc == nil {
		return nil
	}
	data, _ := scc.MarshalJSON()
	var v map[string]interface{}
	_ = json.Unmarshal(data, &v)
	if _, ok := v["supplementalGroups"]; !ok {
		return nil
	}
	data, _ = json.Marshal(v["supplementalGroups"])
	supplementalGroups := SupplementalGroupsStrategyOptions{}
	_ = json.Unmarshal(data, &supplementalGroups)
	if supplementalGroups.Type != v1beta1.SupplementalGroupsStrategyMustRunAs {
		return nil
	}
	return supplementalGroups.Ranges
}

// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package urlmap

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/diff"
	ingresslb "k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/utils"

	"github.com/GoogleCloudPlatform/k8s-multicluster-ingress/app/kubemci/pkg/gcp/backendservice"
	utilsnamer "github.com/GoogleCloudPlatform/k8s-multicluster-ingress/app/kubemci/pkg/gcp/namer"
	"github.com/GoogleCloudPlatform/k8s-multicluster-ingress/app/kubemci/pkg/gcp/status"
)

const (
	// The gce api uses the name of a path rule to match a host rule.
	// TODO(nikhiljindal): Refactor to share with kubernetes/ingress-gce which has the same constant.
	hostRulePrefix = "host"
)

// Syncer manages GCP url maps for multicluster GCP L7 load balancers.
type Syncer struct {
	namer *utilsnamer.Namer
	// Instance of URLMapProvider interface for calling GCE URLMap APIs.
	// There is no separate URLMapProvider interface, so we use the bigger LoadBalancers interface here.
	ump ingresslb.LoadBalancers
}

// NewURLMapSyncer returns a new instance of syncer.
func NewURLMapSyncer(namer *utilsnamer.Namer, ump ingresslb.LoadBalancers) SyncerInterface {
	return &Syncer{
		namer: namer,
		ump:   ump,
	}
}

// Ensure this implements SyncerInterface.
var _ SyncerInterface = &Syncer{}

// EnsureURLMap ensures that the required url map exists for the given ingress.
// See the interface for more details.
func (s *Syncer) EnsureURLMap(lbName, ipAddress string, clusters []string, ing *v1beta1.Ingress, beMap backendservice.BackendServicesMap, forceUpdate bool) (string, error) {
	fmt.Println("Ensuring url map")
	var err error
	desiredUM, err := s.desiredURLMap(lbName, ipAddress, clusters, ing, beMap)
	if err != nil {
		return "", fmt.Errorf("error %s in computing desired url map", err)
	}
	name := desiredUM.Name
	// Check if url map already exists.
	existingUM, err := s.ump.GetUrlMap(name)
	if err == nil {
		fmt.Println("url map", name, "exists already. Checking if it matches our desired url map", name)
		glog.V(5).Infof("Existing url map: %v\n, desired url map: %v", existingUM, desiredUM)
		// Fingerprint is required (and we get an error if it's not set).
		// TODO(G-Harmon): Figure out how to properly calculate the
		// FP. Using Sha256 returned a googleapi error. We shouldn't use
		// the existing FP when we're changing the object (as it seems
		// like it's used for some oportunistic optimization on the
		// server side).
		desiredUM.Fingerprint = existingUM.Fingerprint
		// URL Map with that name exists already. Check if it matches what we want.
		if urlMapMatches(*desiredUM, *existingUM) {
			// Nothing to do. Desired url map exists already.
			fmt.Println("Desired url map exists already")
			return existingUM.SelfLink, nil
		}
		if forceUpdate {
			return s.updateURLMap(desiredUM)
		}
		fmt.Println("Will not overwrite this differing URL Map without the --force flag.")
		return "", fmt.Errorf("will not overwrite URL Map without --force")
	}
	glog.V(5).Infof("Got error %s while trying to get existing url map %s. Will try to create new one", err, name)
	// TODO: Handle non NotFound errors. We should create only if the error is NotFound.
	// Create the url map.
	return s.createURLMap(desiredUM)
}

// DeleteURLMap deletes the url map that EnsureURLMap would have created.
// See the interface for more details.
func (s *Syncer) DeleteURLMap() error {
	name := s.namer.URLMapName()
	fmt.Println("Deleting url map", name)
	err := s.ump.DeleteUrlMap(name)
	if err != nil {
		if utils.IsHTTPErrorCode(err, http.StatusNotFound) {
			fmt.Println("URL map", name, "does not exist. Nothing to delete")
			return nil
		}
		fmt.Println("Error", err, "in deleting URL map", name)
		return err
	}
	fmt.Println("URL map", name, "deleted successfully")
	return nil
}

// GetLoadBalancerStatus returns the status of the given load balancer if it is stored on the url map.
// See the interface for more details.
func (s *Syncer) GetLoadBalancerStatus(lbName string) (*status.LoadBalancerStatus, error) {
	// Fetch the url map.
	name := s.namer.URLMapName()
	um, err := s.ump.GetUrlMap(name)
	if err == nil {
		return getStatus(um)
	}
	if utils.IsHTTPErrorCode(err, http.StatusNotFound) {
		// Preserve StatusNotFound and return the error as is.
		return nil, err
	}
	return nil, fmt.Errorf("error in fetching url map: %s. Cannot determine status without url map", err)
}

func getStatus(um *compute.UrlMap) (*status.LoadBalancerStatus, error) {
	status, err := status.FromString(um.Description)
	if err != nil {
		return nil, fmt.Errorf("error in parsing url map description %s. Cannot determine status without it", err)
	}
	return status, nil
}

// ListLoadBalancerStatuses returns a list of load balancer status from load balancers that have the status stored on their url maps.
// It ignores the load balancers that dont have status on their url map.
// Returns an error if listing url maps fails.
// See the interface for more details.
func (s *Syncer) ListLoadBalancerStatuses() ([]status.LoadBalancerStatus, error) {
	var maps []*compute.UrlMap
	var err error
	result := []status.LoadBalancerStatus{}
	if maps, err = s.ump.ListUrlMaps(); err != nil {
		err = fmt.Errorf("Error getting url maps: %s", err)
		glog.V(2).Infof("%s\n", err)
		return result, err
	}
	glog.V(5).Infof("maps: %+v", maps)
	for _, item := range maps {
		if strings.HasPrefix(item.Name, "mci1") {
			lbStatus, decodeErr := status.FromString(item.Description)
			if decodeErr != nil {
				// Assume that forwarding rule has the right status for this MCI.
				glog.V(3).Infof("Error decoding load balancer status on url map %s: %s\nAssuming status is stored on forwarding rule. Ignoring the error and continuing.", item.Name, decodeErr)
				continue
			}
			result = append(result, *lbStatus)
		}
	}
	return result, nil
}

// RemoveClustersFromStatus removes the given clusters from the LoadBalancerStatus.
// See the interface for more details.
func (s *Syncer) RemoveClustersFromStatus(clusters []string) error {
	fmt.Println("Removing clusters", clusters, "from url map")
	name := s.namer.URLMapName()
	existingUM, err := s.ump.GetUrlMap(name)
	if err != nil {
		if utils.IsHTTPErrorCode(err, http.StatusNotFound) {
			// Load balancer does not exist.
			// Return that error as is.
			return err
		}
		err := fmt.Errorf("error in fetching existing url map %s: %s", name, err)
		fmt.Println(err)
		return err
	}
	// Remove clusters from the urlmap.
	desiredUM, err := s.desiredURLMapWithoutClusters(existingUM, clusters)
	if err != nil {
		fmt.Println("Error getting desired url map:", err)
		return err
	}
	glog.V(5).Infof("Existing url map: %v\n, desired url map: %v\n", *existingUM, *desiredUM)
	_, err = s.updateURLMap(desiredUM)
	return err
}

func (s *Syncer) updateURLMap(desiredUM *compute.UrlMap) (string, error) {
	name := desiredUM.Name
	fmt.Println("Updating existing url map", name, "to match the desired state")
	err := s.ump.UpdateUrlMap(desiredUM)
	if err != nil {
		return "", err
	}
	fmt.Println("URL Map", name, "updated successfully")
	um, err := s.ump.GetUrlMap(name)
	if err != nil {
		return "", err
	}
	return um.SelfLink, nil
}

func (s *Syncer) createURLMap(desiredUM *compute.UrlMap) (string, error) {
	name := desiredUM.Name
	fmt.Println("Creating url map", name)
	glog.V(5).Infof("Creating url map %v", desiredUM)
	err := s.ump.CreateUrlMap(desiredUM)
	if err != nil {
		return "", err
	}
	fmt.Println("URL Map", name, "created successfully")
	um, err := s.ump.GetUrlMap(name)
	if err != nil {
		return "", err
	}
	return um.SelfLink, nil
}

func urlMapMatches(desiredUM, existingUM compute.UrlMap) bool {
	// Clear output-only fields to do our comparison
	existingUM.CreationTimestamp = ""
	existingUM.Kind = ""
	existingUM.Id = 0
	existingUM.SelfLink = ""
	existingUM.ServerResponse = googleapi.ServerResponse{}

	glog.V(5).Infof("desired UM:\n%+v", desiredUM)
	glog.V(5).Infof("existing UM:\n%+v", existingUM)

	equal := reflect.DeepEqual(existingUM, desiredUM)
	if !equal {
		glog.V(1).Infof("Diff:\n%v", diff.ObjectDiff(desiredUM, existingUM))
	}
	return equal
}

func (s *Syncer) desiredURLMap(lbName, ipAddress string, clusters []string, ing *v1beta1.Ingress, beMap backendservice.BackendServicesMap) (*compute.UrlMap, error) {
	desc, err := desiredStatusString(lbName, "URL map", ipAddress, clusters)
	if err != nil {
		return nil, err
	}

	// Compute the desired url map.
	um := &compute.UrlMap{
		Name:        s.namer.URLMapName(),
		Description: desc,
	}
	gceMap, err := s.ingToURLMap(ing, beMap)
	if err != nil {
		fmt.Println("Error getting URL map from Ingress:", err)
		return nil, err
	}
	um.DefaultService = gceMap.GetDefaultBackend().SelfLink
	if len(gceMap) > 0 {
		// Only create these slices if we have data; otherwise we get a
		// DeepEqual mismatch when comparing to what the server returns.
		um.HostRules = []*compute.HostRule{}
		um.PathMatchers = []*compute.PathMatcher{}
	}

	// Code taken from kubernetes/ingress-gce/L7s.UpdateUrlMap.
	// TODO: Refactor it to share code.
	for hostname, urlToBackend := range gceMap {
		// Create a host rule
		// Create a path matcher
		// Add all given endpoint:backends to pathRules in path matcher
		pmName := getNameForPathMatcher(hostname)
		um.HostRules = append(um.HostRules, &compute.HostRule{
			Hosts:       []string{hostname},
			PathMatcher: pmName,
		})

		pathMatcher := &compute.PathMatcher{
			Name:           pmName,
			DefaultService: um.DefaultService,
			PathRules:      []*compute.PathRule{},
		}

		for expr, be := range urlToBackend {
			pathMatcher.PathRules = append(
				pathMatcher.PathRules, &compute.PathRule{Paths: []string{expr}, Service: be.SelfLink})
		}
		um.PathMatchers = append(um.PathMatchers, pathMatcher)
	}
	return um, nil
}

// desiredStatusString returns the expected LoadBalancerStatus converted to string that can be stored as description based on the given input parameters.
func desiredStatusString(lbName, resourceName, ipAddress string, clusters []string) (string, error) {
	// Sort the clusters so we get a deterministic order.
	sort.Strings(clusters)
	status := status.LoadBalancerStatus{
		Description:      fmt.Sprintf("%s for kubernetes multicluster loadbalancer %s", resourceName, lbName),
		LoadBalancerName: lbName,
		Clusters:         clusters,
		IPAddress:        ipAddress,
	}
	desc, err := status.ToString()
	if err != nil {
		return "", fmt.Errorf("unexpected error in converting status to string: %s", err)
	}
	return desc, nil
}

// desiredURLMapWithoutClusters returns a desired url map based on the given existing map such that the given list of clusters is removed from the status.
func (s *Syncer) desiredURLMapWithoutClusters(existingUM *compute.UrlMap, clustersToRemove []string) (*compute.UrlMap, error) {
	existingStatusStr := existingUM.Description
	newStatusStr, err := status.RemoveClusters(existingStatusStr, clustersToRemove)
	if err != nil {
		return nil, fmt.Errorf("unexpected error in updating status to remove clusters on url map %s: %s", existingUM.Name, err)
	}
	// Shallow copy is fine since we are only changing description.
	desiredUM := existingUM
	desiredUM.Description = newStatusStr
	return desiredUM, nil
}

// ingToURLMap converts an ingress to GCEURLMap (nested map of subdomain: url-regex: gce backend).
// TODO: Copied from kubernetes/ingress-gce with minor changes to print errors
// instead of generating events. Refactor it to make it reusable.
func (s *Syncer) ingToURLMap(ing *v1beta1.Ingress, beMap backendservice.BackendServicesMap) (utils.GCEURLMap, error) {
	hostPathBackend := utils.GCEURLMap{}
	var err error
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			fmt.Println("Ignoring non http ingress rule", rule)
			continue
		}
		pathToBackend := map[string]*compute.BackendService{}
		for _, p := range rule.HTTP.Paths {
			backend, beErr := getBackendService(&p.Backend, ing.Namespace, beMap)
			if beErr != nil {
				fmt.Println("Skipping path", p.Backend, "due to error", beErr)
				err = multierror.Append(err, beErr)
				continue
			}
			// The Ingress spec defines empty path as catch-all, so if a user
			// asks for a single host and multiple empty paths, all traffic is
			// sent to one of the last backend in the rules list.
			path := p.Path
			if path == "" {
				path = ingresslb.DefaultPath
			}
			pathToBackend[path] = backend
		}
		// If multiple hostless rule sets are specified, last one wins
		host := rule.Host
		if host == "" {
			host = ingresslb.DefaultHost
		}
		hostPathBackend[host] = pathToBackend
	}
	var defaultBackend *compute.BackendService
	if ing.Spec.Backend == nil {
		// TODO(nikhiljindal): Be able to create a default backend service.
		// For now, we require users to specify it and generate an error if it's nil.
		// We can't create a url map without a default service, so no point continuing.
		err = multierror.Append(err, fmt.Errorf("unexpected: ing.spec.backend is nil. Multicluster ingress needs a user specified default backend"))
		return nil, err
	}
	defaultBackend, beErr := getBackendService(ing.Spec.Backend, ing.Namespace, beMap)
	if beErr != nil {
		fmt.Printf("Error getting backend service %s: %v", ing.Spec.Backend.ServiceName, beErr)
		err = multierror.Append(err, beErr)
		return nil, err
	}
	hostPathBackend.PutDefaultBackend(defaultBackend)
	return hostPathBackend, err
}

func getBackendService(be *v1beta1.IngressBackend, ns string, beMap backendservice.BackendServicesMap) (*compute.BackendService, error) {
	if be == nil {
		return nil, fmt.Errorf("unexpected: received nil ingress backend")
	}
	backendService := beMap[be.ServiceName]
	if backendService == nil {
		return nil, fmt.Errorf("unexpected: No backend service found for service: %s, must have been an error in ensuring backend services", be.ServiceName)
	}
	return backendService, nil
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
// TODO(nikhiljindal): Copied from kubernetes/ingress-gce. Make it a public method there so that it can be reused.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("%v%v", hostRulePrefix, hex.EncodeToString(hasher.Sum(nil)))
}

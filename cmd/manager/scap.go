/*
Copyright © 2020 Red Hat Inc.

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
package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	mcfgcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/wI2L/jsondiff"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimejson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ComplianceAsCode/compliance-operator/pkg/utils"
	"github.com/antchfx/xmlquery"
	"github.com/itchyny/gojq"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes"
)

const (
	contentFileTimeout          = 3600
	valuePrefix                 = "xccdf_org.ssgproject.content_value_"
	kubeletConfigPathPrefix     = "/kubeletconfig/"
	kubeletConfigRolePathPrefix = "/kubeletconfig/role/"
)

var (
	MoreThanOneObjErr = errors.New("more than one object returned from the filter")
)

// resourceFetcherClients just gathers several needed structs together so we can
// pass them on easily to functions
type resourceFetcherClients struct {
	// Client, right now used to retrieve MCs
	client runtimeclient.Client
	// ClientSet for Gets
	clientset *kubernetes.Clientset
	scheme    *runtime.Scheme
}

// For OpenSCAP content as an XML data stream. Implements ResourceFetcher.
type scapContentDataStream struct {
	resourceFetcherClients
	// Staging objects
	dataStream *xmlquery.Node
	tailoring  *xmlquery.Node
	resources  []utils.ResourcePath
	found      map[string][]byte
}

func NewDataStreamResourceFetcher(scheme *runtime.Scheme, client runtimeclient.Client, clientSet *kubernetes.Clientset) ResourceFetcher {
	return &scapContentDataStream{
		resourceFetcherClients: resourceFetcherClients{
			clientset: clientSet,
			client:    client,
			scheme:    scheme,
		},
	}
}

func (c *scapContentDataStream) LoadSource(path string) error {
	xml, err := c.loadContent(path)
	if err != nil {
		return err
	}
	c.dataStream = xml
	return nil
}

func (c *scapContentDataStream) LoadTailoring(path string) error {
	xml, err := c.loadContent(path)
	if err != nil {
		return err
	}
	c.tailoring = xml
	return nil
}

func (c *scapContentDataStream) loadContent(path string) (*xmlquery.Node, error) {
	f, err := openNonEmptyFile(path)
	if err != nil {
		return nil, err
	}
	// #nosec
	defer f.Close()
	return parseContent(f)
}

func parseContent(f *os.File) (*xmlquery.Node, error) {
	return utils.ParseContent(bufio.NewReader(f))
}

// Returns the file, but only after it has been created by the other init container.
// This avoids a race.
func openNonEmptyFile(filename string) (*os.File, error) {
	readFileTimeoutChan := make(chan *os.File, 1)

	// gosec complains that the file is passed through an evironment variable. But
	// this is not a security issue because none of the files are user-provided
	cleanFileName := filepath.Clean(filename)

	go func() {
		for {
			// Note that we're cleaning the filename path above.
			// #nosec
			file, err := os.Open(cleanFileName)
			if err == nil {
				fileinfo, err := file.Stat()
				// Only try to use the file if it already has contents.
				if err == nil && fileinfo.Size() > 0 {
					readFileTimeoutChan <- file
				}
			} else if !os.IsNotExist(err) {
				fmt.Println(err)
				os.Exit(1)
			}
			time.Sleep(1 * time.Second)
		}
	}()

	select {
	case file := <-readFileTimeoutChan:
		fmt.Printf("File '%s' found, using.\n", filename)
		return file, nil
	case <-time.After(time.Duration(contentFileTimeout) * time.Second):
		fmt.Println("Timeout. Aborting.")
		os.Exit(1)
	}

	// We shouldn't get here.
	return nil, nil
}

func (c *scapContentDataStream) FigureResources(profile string) error {
	// Always stage the clusteroperators/openshift-apiserver object for version detection.
	found := []utils.ResourcePath{
		{
			ObjPath:  "/version",
			DumpPath: "/version",
		},
		{
			ObjPath:  "/apis/config.openshift.io/v1/clusteroperators/openshift-apiserver",
			DumpPath: "/apis/config.openshift.io/v1/clusteroperators/openshift-apiserver",
		},
		{
			ObjPath:  "/apis/config.openshift.io/v1/infrastructures/cluster",
			DumpPath: "/apis/config.openshift.io/v1/infrastructures/cluster",
		},
		{
			ObjPath:  "/apis/config.openshift.io/v1/networks/cluster",
			DumpPath: "/apis/config.openshift.io/v1/networks/cluster",
		},
		{
			ObjPath:  "/api/v1/nodes",
			DumpPath: "/api/v1/nodes",
		},
	}

	roleNodesList, err := fetchNodesWithRole(context.Background(), c.resourceFetcherClients.client)
	if err != nil {
		LOG("Failed to fetch role list with nodes, error: %v", err)
		return err
	}

	if len(roleNodesList) > 0 {
		found = append(found, getKubeletConfigResourcePath(roleNodesList)...)
	}

	effectiveProfile := profile
	var valuesList map[string]string

	if c.tailoring != nil {
		var selected []utils.ResourcePath
		selected, valuesList = getResourcePaths(c.tailoring, c.dataStream, profile, nil)
		if len(selected) == 0 {
			fmt.Printf("no valid checks found in tailoring\n")
		}
		found = append(found, selected...)
		// Overwrite profile so the next search uses the extended profile
		effectiveProfile = c.getExtendedProfileFromTailoring(c.tailoring, profile)
		// No profile is being extended
		if effectiveProfile == "" {
			c.resources = found
			return nil
		}
	}

	selected, _ := getResourcePaths(c.dataStream, c.dataStream, effectiveProfile, valuesList)
	if len(selected) == 0 {
		fmt.Printf("no valid checks found in profile\n")
	}
	found = append(found, selected...)
	c.resources = found
	DBG("c.resources: %v\n", c.resources)
	return nil
}

// getPathsFromRuleWarning finds the API endpoint from in. The expected structure is:
//
//	<warning category="general" lang="en-US"><code class="ocp-api-endpoint">/apis/config.openshift.io/v1/oauths/cluster
//	</code></warning>
func getPathFromWarningXML(in *xmlquery.Node, valueList map[string]string) []utils.ResourcePath {
	DBG("Parsing warning %s", in.OutputXML(false))
	path, err := utils.GetPathFromWarningXML(in, valueList)
	if err != nil {
		LOG("Error occurred at parsing warning %s", in.OutputXML((false)))
		LOG("Error message: %s", err)
	}
	return path
}

// Fetch all nodes from the cluster and find all roles for each node.
func fetchNodesWithRole(ctx context.Context, c runtimeclient.Client) (map[string][]string, error) {
	nodeList := v1.NodeList{}
	if err := c.List(ctx, &nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	roleNodesList := make(map[string][]string)
	for _, node := range nodeList.Items {
		nodeName := node.Name
		nodeRoles := utils.GetNodeRoles(node.ObjectMeta.Labels)
		for _, role := range nodeRoles {
			roleNodesList[role] = append(roleNodesList[role], nodeName)
		}
	}

	return roleNodesList, nil

}

// Get resourcePath for KubeletConfig
func getKubeletConfigResourcePath(roleNodesList map[string][]string) []utils.ResourcePath {
	resourcePath := []utils.ResourcePath{}
	for role, nodeList := range roleNodesList {
		for _, node := range nodeList {
			resourcePath = append(resourcePath, utils.ResourcePath{
				ObjPath:  "/api/v1/nodes/" + node + "/proxy/configz",
				DumpPath: kubeletConfigPathPrefix + role + "/" + node,
				Filter:   `.kubeletconfig|.kind="KubeletConfiguration"|.apiVersion="kubelet.config.k8s.io/v1beta1"`,
			})
		}
	}
	return resourcePath
}

// Get role name and node name from DumpPath
func getRoleNodeNameFromDumpPath(dumpPath string) (roleName string, nodeName string) {
	if strings.HasPrefix(dumpPath, kubeletConfigPathPrefix) {
		dumpPathSplit := strings.Split(dumpPath, "/")
		if len(dumpPathSplit) != 4 {
			return "", ""
		}
		// example of DumpPath: /kubeletconfig/master/node-1
		roleName = dumpPathSplit[2]
		nodeName = dumpPathSplit[3]
		return roleName, nodeName
	}
	return "", ""
}

// Collect the resource paths for objects that this scan needs to obtain.
// The profile will have a series of "selected" checks that we grab all of the path info from.
func getResourcePaths(profileDefs *xmlquery.Node, ruleDefs *xmlquery.Node, profile string, overrideValueList map[string]string) ([]utils.ResourcePath, map[string]string) {
	out := []utils.ResourcePath{}
	selectedChecks := []string{}

	// Before staring process, collect all of the variables in definitions.
	valuesList := make(map[string]string)
	defs := [...]*xmlquery.Node{ruleDefs, profileDefs}
	for _, def := range defs {
		allValues := xmlquery.Find(def, "//xccdf-1.2:Value")

		for _, variable := range allValues {
			for _, val := range variable.SelectElements("//xccdf-1.2:value") {
				if val.SelectAttr("hidden") == "true" {
					// this is typically used for functions
					continue
				}
				if val.SelectAttr("selector") == "" {
					// It is not an enum choice, but a default value instead
					if strings.HasPrefix(variable.SelectAttr("id"), valuePrefix) {
						valuesList[strings.TrimPrefix(variable.SelectAttr("id"), valuePrefix)] = html.UnescapeString(val.OutputXML(false))
					}
				}
			}
		}
		allSetValues := xmlquery.Find(def, "//xccdf-1.2:set-value")
		for _, variable := range allSetValues {
			if strings.HasPrefix(variable.SelectAttr("idref"), valuePrefix) {
				valuesList[strings.TrimPrefix(variable.SelectAttr("idref"), valuePrefix)] = html.UnescapeString(variable.OutputXML(false))
			}
		}
	}

	// override variables which is defined in tailored profile
	if overrideValueList != nil {
		for k, v := range overrideValueList {
			if _, exists := valuesList[k]; exists {
				valuesList[k] = v
			}
		}
	}

	// First we find the Profile node, to locate the enabled checks.
	DBG("Using profile %s", profile)
	nodes := profileDefs.SelectElements("//xccdf-1.2:Profile")
	if len(nodes) == 0 {
		DBG("no profiles found in datastream")
	}
	for _, node := range nodes {
		profileID := node.SelectAttr("id")
		if profileID != profile {
			continue
		}

		checks := node.SelectElements("//xccdf-1.2:select")
		for _, check := range checks {
			if check.SelectAttr("selected") != "true" {
				continue
			}

			if idRef := check.SelectAttr("idref"); idRef != "" {
				DBG("selected: %v", idRef)
				selectedChecks = append(selectedChecks, idRef)
			}
		}
	}

	checkDefinitions := ruleDefs.SelectElements("//xccdf-1.2:Rule")
	if len(checkDefinitions) == 0 {
		DBG("WARNING: No rules to query (invalid datastream)")
		return out, valuesList
	}

	// For each of our selected checks, collect the required path info.
	for _, checkID := range selectedChecks {
		var found *xmlquery.Node
		for _, rule := range checkDefinitions {
			if rule.SelectAttr("id") == checkID {
				found = rule
				break
			}
		}
		if found == nil {
			DBG("WARNING: Couldn't find a check for id %s", checkID)
			continue
		}

		// This node is called "warning" and contains the path info. It's not an actual "warning" for us here.
		var warningFound bool
		warningObjs := found.SelectElements("//xccdf-1.2:warning")

		for _, warn := range warningObjs {
			if warn == nil {
				continue
			}
			apiPaths := getPathFromWarningXML(warn, valuesList)
			if len(apiPaths) == 0 {
				continue
			}
			// We only care for the first occurrence that works
			out = append(out, apiPaths...)
			warningFound = true
			break
		}

		if !warningFound {
			DBG("Couldn't find 'warning' child of check %s", checkID)
			continue
		}

	}
	return out, valuesList
}

func (c *scapContentDataStream) getExtendedProfileFromTailoring(ds *xmlquery.Node, tailoredProfile string) string {
	nodes := ds.SelectElements("//xccdf-1.2:Profile")
	for _, node := range nodes {
		tailoredProfileID := node.SelectAttr("id")
		if tailoredProfileID != tailoredProfile {
			continue
		}

		profileID := node.SelectAttr("extends")
		if profileID != "" {
			return profileID
		}
	}
	return ""
}

func (c *scapContentDataStream) FetchResources() ([]string, error) {
	found, warnings, err := fetch(context.Background(), getStreamerFn, c.resourceFetcherClients, c.resources)
	if err != nil {
		return warnings, err
	}
	c.found = found
	return warnings, nil
}

// resourceStreamer is an interface capable of streaming a particular URI
type resourceStreamer interface {
	Stream(ctx context.Context, rfClients resourceFetcherClients) (io.ReadCloser, error)
}

type streamerDispatcherFn func(string) resourceStreamer

// getStreamerFn returns a structure implementing resourceStreamer interface based on the
// uri passed to it
func getStreamerFn(uri string) resourceStreamer {
	if uri == "/apis/machineconfiguration.openshift.io/v1/machineconfigs" {
		return &mcStreamer{}
	}

	return &uriStreamer{
		uri: uri,
	}
}

// uriStreamer implements resourceStreamer for fetching a generic URI
type uriStreamer struct {
	uri string
}

func (us *uriStreamer) Stream(ctx context.Context, rfClients resourceFetcherClients) (io.ReadCloser, error) {
	return rfClients.clientset.RESTClient().Get().RequestURI(us.uri).Stream(ctx)
}

// mcStreamer implements resourceStreamer for fetching a list of MachineConfigs
type mcStreamer struct{}

// bufCloser is a kludge so that mcStreamer's Stream() method can return an io.ReadCloser
type bufCloser struct {
	*bytes.Buffer
}

// Close is a dummy method because a buffer doesn't have to be closed, just enables us to return io.ReadCloser
// from mcStreamer's Stream() method
func (bc *bufCloser) Close() error {
	return nil
}

// Stream fetches MachineConfigs in batches of pageSize, removes the file contents from each MC in the batch,
// adds each batch to a resulting list which is finally returned as JSON
func (ms *mcStreamer) Stream(ctx context.Context, rfClients resourceFetcherClients) (io.ReadCloser, error) {
	mcfgListNoFiles := mcfgv1.MachineConfigList{}
	const pageSize = 5

	continueToken := ""
	for {
		mcfgList := mcfgv1.MachineConfigList{}
		listOpts := runtimeclient.ListOptions{
			Limit: int64(pageSize),
		}
		if continueToken != "" {
			listOpts.Continue = continueToken
		}
		if err := rfClients.client.List(ctx, &mcfgList, &listOpts); err != nil {
			return nil, fmt.Errorf("failed to list MachineConfigs: %w", err)
		}

		mcfgListNoFilesBatch, err := filterMcList(&mcfgList)
		if err != nil {
			return nil, fmt.Errorf("failed to filter machine configs: %w", err)
		}

		mcfgListNoFiles.Items = append(mcfgListNoFiles.Items, mcfgListNoFilesBatch.Items...)

		continueToken = mcfgList.ListMeta.Continue
		if continueToken == "" {
			break
		}
	}

	jsonSerializer := runtimejson.NewSerializerWithOptions(runtimejson.DefaultMetaFactory,
		rfClients.scheme,
		rfClients.scheme,
		runtimejson.SerializerOptions{Pretty: true})
	buf := &bufCloser{&bytes.Buffer{}}
	if err := jsonSerializer.Encode(&mcfgListNoFiles, buf); err != nil {
		return nil, fmt.Errorf("failed to serialize MC list: %w", err)
	}
	return buf, nil
}

func filterMcList(mcListIn *mcfgv1.MachineConfigList) (*mcfgv1.MachineConfigList, error) {
	mcfgListNoFiles := mcfgv1.MachineConfigList{}
	mcfgListNoFiles.TypeMeta = mcListIn.TypeMeta
	mcfgListNoFiles.ListMeta = mcListIn.ListMeta

	for i := 0; i < len(mcListIn.Items); i++ {
		mc := mcListIn.Items[i]
		if len(mc.Spec.Config.Raw) > 0 {
			// if Ignition exists, filter out all the potentially large files
			ign, err := mcfgcommon.ParseAndConvertConfig(mc.Spec.Config.Raw)
			if err != nil {
				return nil, fmt.Errorf("cannot parse MC %s: %w", mc.Name, err)
			}
			ign.Storage.Files = nil // just get rid of the files the easy way
			rawOutIgn, err := json.Marshal(ign)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal Ignition object back to a a raw object: %w", err)
			}
			mc.Spec.Config.Raw = rawOutIgn
		}
		mcfgListNoFiles.Items = append(mcfgListNoFiles.Items, mc)
	}

	return &mcfgListNoFiles, nil
}

func fetch(ctx context.Context, streamDispatcher streamerDispatcherFn, rfClients resourceFetcherClients, objects []utils.ResourcePath) (map[string][]byte, []string, error) {
	var warnings []string
	results := map[string][]byte{}

	for _, rpath := range objects {
		err := func() error {
			uri := rpath.ObjPath
			LOG("Fetching URI: '%s'", uri)
			streamer := streamDispatcher(uri)
			stream, err := streamer.Stream(ctx, rfClients)
			if meta.IsNoMatchError(err) || kerrors.IsForbidden(err) || kerrors.IsNotFound(err) {
				DBG("Encountered non-fatal error to be persisted in the scan: %s", err)
				objerr := fmt.Errorf("could not fetch %s: %w", uri, err)
				warnings = append(warnings, objerr.Error())
				// for 404s we'll add a warning comment in the object so openSCAP can read and process it
				if kerrors.IsNotFound(err) {
					results[rpath.DumpPath] = []byte("# kube-api-error=" + kerrors.ReasonForError(err))
				}
				return nil
			} else if err != nil {
				return fmt.Errorf("streaming URIs failed: %w", err)
			}
			defer stream.Close()
			body, err := ioutil.ReadAll(stream)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				DBG("no data in request body")
				return nil
			}
			if rpath.Filter != "" {
				DBG("Applying filter '%s' to path '%s'", rpath.Filter, rpath.ObjPath)
				filteredBody, filterErr := filter(ctx, body, rpath.Filter)
				if errors.Is(filterErr, MoreThanOneObjErr) {
					warnings = append(warnings, filterErr.Error())
				} else if filterErr != nil {
					return fmt.Errorf("couldn't filter '%s': %w", body, filterErr)
				}
				results[rpath.DumpPath] = filteredBody
			} else {
				results[rpath.DumpPath] = body
			}
			return nil
		}()
		if err != nil {
			return nil, warnings, err
		}
	}
	results, warnings, err := saveConsistentKubeletResult(results, warnings)
	return results, warnings, err
}

// Only save consistent KubeletConfigs per node role.
func saveConsistentKubeletResult(result map[string][]byte, warning []string) (map[string][]byte, []string, error) {
	if len(result) == 0 {
		return result, warning, nil
	}
	kubeletConfigsRole := make(map[string][]byte)
	for dumpPath, content := range result {
		role, node := getRoleNodeNameFromDumpPath(dumpPath)
		if role == "" {
			continue
		}
		if existingKC, ok := kubeletConfigsRole[role]; ok {
			diff, err := jsondiff.CompareJSON(existingKC, content)
			if err != nil {
				return nil, nil, fmt.Errorf("couldn't compare kubelet configs: %w for %s", err, node)
			}
			if diff != nil {
				why := fmt.Sprintf("Kubelet configs for %s are not consistent with role %s, Diff: %s of KubeletConfigs for %s role will not be saved.", node, role, diff, role)
				LOG(why)
				warning = append(warning, why)
				intersectionKC, err := utils.JSONIntersection(existingKC, content)
				if err != nil {
					return nil, nil, fmt.Errorf("couldn't get intersection of kubelet configs: %w for %s", err, node)
				}
				kubeletConfigsRole[role] = intersectionKC
			}
		} else {
			kubeletConfigsRole[role] = content
		}
	}
	for role, content := range kubeletConfigsRole {
		if role == "" {
			continue
		}
		result[kubeletConfigRolePathPrefix+role] = content
	}
	return result, warning, nil
}

func filter(ctx context.Context, rawobj []byte, filter string) ([]byte, error) {
	fltr, fltrErr := gojq.Parse(filter)
	if fltrErr != nil {
		return nil, fmt.Errorf("could not create filter '%s': %w", filter, fltrErr)
	}
	obj := map[string]interface{}{}
	unmarshallErr := json.Unmarshal(rawobj, &obj)
	if unmarshallErr != nil {
		return nil, fmt.Errorf("Error unmarshalling json: %w", unmarshallErr)
	}
	iter := fltr.RunWithContext(ctx, obj)
	v, ok := iter.Next()
	if !ok {
		DBG("No result from filter. This is an issue and an error will be returned.")
		return nil, fmt.Errorf("couldn't get filtered object")
	}
	if err, ok := v.(error); ok {
		DBG("Error while filtering: %s", err)
		return nil, err
	}

	out, marshallErr := json.Marshal(&v)
	if marshallErr != nil {
		return nil, fmt.Errorf("Error marshalling json: %w", marshallErr)
	}
	_, isNotEOF := iter.Next()
	if isNotEOF {
		DBG("No more results should have come from the filter. This is an issue with the content.")
		return out, fmt.Errorf("Skipping extra results from filter '%s': %w", filter, MoreThanOneObjErr)
	}
	return out, nil
}

func (c *scapContentDataStream) SaveWarningsIfAny(warnings []string, outputFile string) error {
	// No warnings to persist
	if warnings == nil || len(warnings) == 0 {
		return nil
	}
	DBG("Persisting warnings to output file")
	warningsStr := strings.Join(warnings, "\n")
	err := ioutil.WriteFile(outputFile, []byte(warningsStr), 0600)
	return err
}

func (c *scapContentDataStream) SaveResources(to string) error {
	return saveResources(to, c.found)
}

func saveResources(rootDir string, data map[string][]byte) error {
	for apiPath, fileContents := range data {
		saveDir, saveFile, err := getSaveDirectoryAndFileName(rootDir, apiPath)
		savePath := path.Join(saveDir, saveFile)
		LOG("Saving fetched resource to: '%s'", savePath)
		if err != nil {
			return err
		}
		err = os.MkdirAll(saveDir, 0700)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(savePath, fileContents, 0600)
		if err != nil {
			return err
		}
	}
	return nil
}

// Returns the absolute directory path (including rootDir) and filename for the given apiPath.
func getSaveDirectoryAndFileName(rootDir string, apiPath string) (string, string, error) {
	base := path.Base(apiPath)
	if base == "." || base == "/" {
		return "", "", fmt.Errorf("bad object path: %s", apiPath)
	}
	subDirs := path.Dir(apiPath)
	if subDirs == "." {
		return "", "", fmt.Errorf("bad object path: %s", apiPath)
	}

	return path.Join(rootDir, subDirs), base, nil
}

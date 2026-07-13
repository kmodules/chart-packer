/*
Copyright AppsCode Inc. and Contributors

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

package cmds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	crdv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

func NewCmdGenerateCRDOnlyChart() *cobra.Command {
	var (
		input  string
		output string
		semver = true
	)
	cmd := &cobra.Command{
		Use:                   "crd-only",
		Short:                 "Generate crd only chart",
		DisableFlagsInUseLine: true,
		DisableAutoGenTag:     true,
		Run: func(cmd *cobra.Command, args []string) {
			// Load the chart (supports directory or .tgz)
			ch, err := loader.Load(input)
			if err != nil {
				fmt.Printf("Error loading chart: %v\n", err)
				os.Exit(1)
			}
			newChartName := ch.Metadata.Name + "-certified-crds"

			// Collect every CRD in the chart tree. The parent chart's CRDObjects()
			// already recurses into all subcharts, so a single call captures them
			// all. Each candidate remembers its source chart so that, when the same
			// GroupKind is provided by more than one chart, the winner can be picked
			// deterministically below.
			var candidates []crdCandidate
			for _, c := range ch.CRDObjects() {
				key, err := extractCRDKey(c.File.Data)
				if err != nil {
					fmt.Printf("Warning: Failed to parse CRD %s: %v\n", c.Filename, err)
					continue
				}
				sum := sha256.Sum256(c.File.Data)
				candidates = append(candidates, crdCandidate{
					key:       *key,
					chartPath: strings.TrimSuffix(c.Filename, "/"+c.Name),
					filename:  c.Filename,
					digest:    hex.EncodeToString(sum[:]),
					file:      c.File,
				})
			}

			// Order candidates deterministically so the same input always yields the
			// same output, independent of Helm's dependency iteration order (which is
			// not guaranteed stable and varies with how subcharts were loaded):
			//   1. group, then kind     -> stable output file order
			//   2. chart depth          -> the parent chart wins over its subcharts
			//   3. chart path, filename -> stable tiebreak among subcharts
			//   4. content digest       -> final tiebreak when the same GroupKind is
			//                              provided by two sources that share a path
			//                              (e.g. a directory and a .tgz of one subchart,
			//                              or a diamond dependency) yet differ in content
			sort.Slice(candidates, func(i, j int) bool {
				a, b := candidates[i], candidates[j]
				if a.key.Group != b.key.Group {
					return a.key.Group < b.key.Group
				}
				if a.key.Kind != b.key.Kind {
					return a.key.Kind < b.key.Kind
				}
				if da, db := chartDepth(a.chartPath), chartDepth(b.chartPath); da != db {
					return da < db
				}
				if a.chartPath != b.chartPath {
					return a.chartPath < b.chartPath
				}
				if a.filename != b.filename {
					return a.filename < b.filename
				}
				return a.digest < b.digest
			})

			// The first candidate for each GroupKind wins; the rest are dropped. Since
			// candidates are already sorted by (group, kind), crdFiles comes out in a
			// stable order too — no further sorting needed. A warning is emitted only
			// when a dropped copy actually differs in content from the winner, so that
			// identical duplicates (the common case) stay quiet while genuine version
			// conflicts remain visible.
			winners := make(map[schema.GroupKind]crdCandidate)
			crdFiles := make([]*chart.File, 0, len(candidates))
			for _, cand := range candidates {
				if winner, exists := winners[cand.key]; exists {
					if cand.digest != winner.digest {
						fmt.Printf("Warning: CRD %s/%s conflicts between %s and %s — keeping version from %s\n",
							cand.key.Kind, cand.key.Group, winner.chartPath, cand.chartPath, winner.chartPath)
					}
					continue
				}
				winners[cand.key] = cand
				crdFiles = append(crdFiles, cand.file)
			}

			var extraFiles []*chart.File

			// Collect additional files from the main chart only
			filesToCopy := []string{
				"doc.yaml",
				"README.md",
				"values.yaml",
				"values.schema.json",
				".helmignore",
			}
			for _, name := range filesToCopy {
				for _, f := range ch.Raw {
					if f.Name == name {
						if name == "doc.yaml" {
							if data, err := modifyDocYaml(f.Data, newChartName); err != nil {
								fmt.Printf("Warning: Failed to modify doc.yaml: %v\n", err)
							} else {
								extraFiles = append(extraFiles, &chart.File{
									Name: f.Name,
									Data: data,
								})
							}
						} else {
							extraFiles = append(extraFiles, f)
						}
						break
					}
				}
			}

			// Save templates helpers
			for _, f := range ch.Templates {
				if strings.HasPrefix(f.Name, "templates/_") {
					extraFiles = append(extraFiles, f)
				}
			}

			// Combine CRDs and extra files
			allFiles := append(crdFiles, extraFiles...)

			// Create new minimal chart containing only CRDs
			newChart := &chart.Chart{
				Metadata: &chart.Metadata{
					Name:        newChartName,
					Version:     ch.Metadata.Version,
					Description: "Chart containing only CRDs from " + ch.Name() + " chart",
					APIVersion:  chart.APIVersionV2,
					Home:        ch.Metadata.Home,
					Sources:     ch.Metadata.Sources,
					Keywords:    ch.Metadata.Keywords,
					Maintainers: ch.Metadata.Maintainers,
					Icon:        ch.Metadata.Icon,
					Condition:   ch.Metadata.Condition,
					Tags:        ch.Metadata.Tags,
					AppVersion:  ch.Metadata.AppVersion,
					Annotations: ch.Metadata.Annotations,
					KubeVersion: ch.Metadata.KubeVersion,
				},
				Files: allFiles,
			}
			renameChart(newChart, newChartName)
			if semver {
				newChart.Metadata.Version = strings.TrimPrefix(ch.Metadata.Version, "v")
			}

			// Save to output directory
			if err := chartutil.SaveDir(newChart, output); err != nil {
				fmt.Printf("Error saving repackaged chart: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Successfully repackaged %d unique CRDs + %d additional files into %s\n",
				len(crdFiles), len(extraFiles), output)
		},
	}

	cmd.Flags().StringVar(&input, "input", "", "Path to the input Helm chart directory or .tgz file")
	cmd.Flags().StringVar(&output, "output", "", "Output directory for the repackaged CRDs-only chart")
	cmd.Flags().BoolVar(&semver, "semver", semver, "If true, use strict semver version (no v prefix)")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "input")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "output")

	return cmd
}

// crdCandidate is a single CRD file discovered somewhere in the chart tree,
// tagged with enough source information to pick a deterministic winner when the
// same GroupKind is provided by multiple charts.
type crdCandidate struct {
	key       schema.GroupKind
	chartPath string // dotted chart path of the source chart, e.g. "kubedb" or "kubedb/charts/petset"
	filename  string // full path of the CRD file within the chart tree, used as a tiebreak
	digest    string // sha256 of the file content, the final deterministic tiebreak
	file      *chart.File
}

// chartDepth reports how deeply a chart is nested within the tree: 0 for the
// top-level chart, 1 for its direct subcharts, and so on. Helm builds a
// subchart's full path as "<parent>/charts/<name>", so counting the "/charts/"
// separators yields the nesting depth.
func chartDepth(chartPath string) int {
	return strings.Count(chartPath, "/charts/")
}

// extractCRDKey parses the YAML CRD and builds a unique key
func extractCRDKey(data []byte) (*schema.GroupKind, error) {
	var crd crdv1.CustomResourceDefinition

	if err := yaml.Unmarshal(data, &crd); err != nil {
		return nil, err
	}

	if crd.APIVersion == "" || crd.Kind != "CustomResourceDefinition" {
		return nil, fmt.Errorf("not a valid CustomResourceDefinition")
	}

	return &schema.GroupKind{
		Group: crd.Spec.Group,
		Kind:  crd.Spec.Names.Kind,
	}, nil
}

// modifyDocYaml replaces common placeholders like {{ .Release.Name }} and {{ .Chart.Name }} with the new fixed name
func modifyDocYaml(data []byte, newChartName string) ([]byte, error) {
	var content map[string]any
	if err := yaml.Unmarshal(data, &content); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(content, newChartName, "project", "name"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(content, newChartName, "project", "shortName"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(content, newChartName, "chart", "name"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(content, newChartName, "release", "name"); err != nil {
		return nil, err
	}
	return yaml.Marshal(content)
}

func renameChart(ch *chart.Chart, newChartName string) {
	ch.Metadata.Name = newChartName
	_, ok := ch.Metadata.Annotations["charts.openshift.io/name"]
	if ok {
		ch.Metadata.Annotations["charts.openshift.io/name"] = newChartName
	}
}

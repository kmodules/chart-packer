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
	"fmt"
	"os"
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

			// Map to store unique CRDs: key is (group, kind, plural), value is the chart.File and source chart name
			crdMap := make(map[schema.GroupKind]*chart.File)
			sourceMap := make(map[schema.GroupKind]string) // for warning messages

			// First: collect CRDs from the main (parent) chart — these take precedence
			collectCRDs(ch, ch.Name(), crdMap, sourceMap)

			// Then: collect from all dependencies (subcharts)
			for _, dep := range ch.Dependencies() {
				if dep != nil {
					collectCRDs(dep, dep.Name(), crdMap, sourceMap)
				}
			}

			// Convert to slice
			var crdFiles []*chart.File
			for _, file := range crdMap {
				crdFiles = append(crdFiles, file)
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

func collectCRDs(ch *chart.Chart, sourceName string, crdMap map[schema.GroupKind]*chart.File, sourceMap map[schema.GroupKind]string) {
	for _, f := range ch.CRDObjects() {
		key, err := extractCRDKey(f.File.Data)
		if err != nil {
			fmt.Printf("Warning: Failed to parse CRD %s from %s: %v\n", f.Name, sourceName, err)
			continue
		}

		if existingSource, exists := sourceMap[*key]; exists {
			fmt.Printf("Warning: CRD %s/%s duplicated in %s — keeping version from %s\n",
				key.Kind, key.Group, sourceName, existingSource)
			continue
		}

		// New unique CRD
		crdMap[*key] = f.File
		sourceMap[*key] = sourceName
	}
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

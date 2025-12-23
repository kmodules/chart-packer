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
)

func NewCmdGenerateCRDLessChart() *cobra.Command {
	var input string
	var output string
	cmd := &cobra.Command{
		Use:                   "crd-less",
		Short:                 "Generate crd less chart",
		DisableFlagsInUseLine: true,
		DisableAutoGenTag:     true,
		Run: func(cmd *cobra.Command, args []string) {
			// Load the chart archive directly using Helm SDK
			ch, err := loader.Load(input)
			if err != nil {
				fmt.Printf("Error loading chart archive: %v\n", err)
				os.Exit(1)
			}
			newChartName := ch.Metadata.Name + "-certified"

			// Remove CRDs from the main chart and recursively from dependencies
			removeCRDsFromChart(ch)

			ch.Metadata.Name = newChartName

			for _, f := range ch.Files {
				if f.Name == "doc.yaml" {
					if data, err := modifyDocYaml(f.Data, newChartName); err != nil {
						fmt.Printf("Warning: Failed to modify doc.yaml: %v\n", err)
					} else {
						f.Data = data
					}
					break
				}
			}
			// Save the modified chart to the output tgz
			if err := chartutil.SaveDir(ch, output); err != nil {
				fmt.Printf("Error saving modified chart: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Repackaged chart without CRDs to %s\n", output)
		},
	}

	cmd.Flags().StringVar(&input, "input", "/Users/tamal/go/src/kubedb.dev/installer/charts/kubedb", "input helm chart tgz file")
	cmd.Flags().StringVar(&output, "output", "/Users/tamal/go/src/kubedb.dev/fg/repack", "output helm chart tgz file without CRDs")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "input")
	_ = cobra.MarkFlagRequired(cmd.Flags(), "output")

	return cmd
}

// removeCRDsFromChart removes all files under 'crds/' directory in the given chart
// and recursively processes any dependency subcharts (both embedded directory and archived).
func removeCRDsFromChart(ch *chart.Chart) {
	// Remove CRD files from main chart
	newFiles := make([]*chart.File, 0, len(ch.Files))
	for _, f := range ch.Files {
		if !strings.HasPrefix(f.Name, "crds/") {
			newFiles = append(newFiles, f)
		}
	}
	ch.Files = newFiles

	// Also clear Raw if present (though usually not in archived charts)
	// ch.Raw = nil

	// Process dependencies
	newDeps := make([]*chart.Chart, 0, len(ch.Dependencies()))
	for _, dep := range ch.Dependencies() {
		if dep == nil {
			continue
		}

		// If the dependency is an embedded archive (common in packaged charts)
		if dep.Metadata != nil && len(dep.Files) > 0 {
			// Recursively remove CRDs from this subchart
			removeCRDsFromChart(dep)
			newDeps = append(newDeps, dep)
			continue
		}

		// If it's a directory-based dependency (rare in tgz, but possible)
		// We skip further recursion here as packaged charts usually embed archives.
		newDeps = append(newDeps, dep)
	}
	ch.SetDependencies(newDeps...)
}

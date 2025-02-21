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

package main

import (
	"fmt"
	"github.com/ComplianceAsCode/compliance-operator/cmd/manager"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "compliance-operator",
	Short: "An operator that issues compliance checks and their lifecycle.",
	Long:  `An operator that issues compliance checks and their lifecycle.`,
	Run:   func(cmd *cobra.Command, args []string) {},
}

func init() {
	rootCmd.AddCommand(manager.OperatorCmd)
	rootCmd.AddCommand(manager.AggregatorCmd)
	rootCmd.AddCommand(manager.ApiResourceCollectorCmd)
	rootCmd.AddCommand(manager.ProfileparserCmd)
	rootCmd.AddCommand(manager.ResultcollectorCmd)
	rootCmd.AddCommand(manager.ResultServerCmd)
	rootCmd.AddCommand(manager.RerunnerCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

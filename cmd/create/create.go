/*
Copyright © 2019 Thorsten Klein <iwilltry42@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package create

import (
	"github.com/spf13/cobra"
)

// NewCmdCreate returns a new cobra command
func NewCmdCreate() *cobra.Command {

	// create new cobra command
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a resource [cluster, node].",
		Long:  `Create a resource [cluster, node].`,
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}

	// add subcommands
	cmd.AddCommand(NewCmdCreateCluster())
	cmd.AddCommand(NewCmdCreateNode())

	// add flags
	cmd.PersistentFlags().StringP("runtime", "r", "docker", "Choose a container runtime environment [docker, containerd]")

	// done
	return cmd
}

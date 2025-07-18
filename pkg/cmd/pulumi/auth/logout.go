// Copyright 2016-2024, Pulumi Corporation.
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

package auth

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/pulumi/pulumi/pkg/v3/backend/httpstate"
	pkgWorkspace "github.com/pulumi/pulumi/pkg/v3/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/common/env"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

func NewLogoutCmd(ws pkgWorkspace.Context) *cobra.Command {
	var cloudURL string
	var localMode bool
	var all bool

	cmd := &cobra.Command{
		Use:   "logout <url>",
		Short: "Log out of the Pulumi Cloud",
		Long: "Log out of the Pulumi Cloud.\n" +
			"\n" +
			"This command deletes stored credentials on the local machine for a single login.\n" +
			"\n" +
			"Because you may be logged into multiple backends simultaneously, you can optionally pass\n" +
			"a specific URL argument, formatted just as you logged in, to log out of a specific one.\n" +
			"If no URL is provided, you will be logged out of the current backend." +
			"\n\n" +
			"If you would like to log out of all backends simultaneously, you can pass `--all`,\n\n" +
			"    $ pulumi logout --all",
		Args: cmdutil.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// If a <cloud> was specified as an argument, use it.
			if len(args) > 0 {
				if cloudURL != "" || all {
					return errors.New("only one of --all, --cloud-url or argument URL may be specified, not both")
				}
				cloudURL = args[0]
			}

			// For local mode, store state by default in the user's home directory.
			if localMode {
				if cloudURL != "" {
					return errors.New("a URL may not be specified when --local mode is enabled")
				}
				cloudURL = "file://~"
			}

			var err error
			if all {
				err = workspace.DeleteAllAccounts()
				fmt.Println("Logged out of everything")
			} else {
				if cloudURL == "" {
					// Try to read the current project
					project, _, err := ws.ReadProject()
					if err != nil && !errors.Is(err, workspace.ErrProjectNotFound) {
						return err
					}

					cloudURL, err = pkgWorkspace.GetCurrentCloudURL(ws, env.Global(), project)
					if err != nil {
						return fmt.Errorf("could not determine current cloud: %w", err)
					}

					// Default to the default cloud URL. This means a `pulumi logout` will delete the
					// credentials for pulumi.com if there's no "current" user set in the credentials file.
					cloudURL = httpstate.ValueOrDefaultURL(ws, cloudURL)
				}

				err = workspace.DeleteAccount(cloudURL)
				fmt.Printf("Logged out of %s\n", cloudURL)
			}

			return err
		},
	}

	cmd.PersistentFlags().BoolVar(&all, "all", false,
		"Logout of all backends")
	cmd.PersistentFlags().StringVarP(&cloudURL, "cloud-url", "c", "",
		"A cloud URL to log out of (defaults to current cloud)")
	cmd.PersistentFlags().BoolVarP(&localMode, "local", "l", false,
		"Log out of using local mode")

	return cmd
}

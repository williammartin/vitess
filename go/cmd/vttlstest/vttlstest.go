/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"github.com/spf13/cobra"

	"vitess.io/vitess/go/cmd/vttlstest/cli"
	"vitess.io/vitess/go/exit"
	"vitess.io/vitess/go/vt/logutil"
)

func main() {
	defer exit.Recover()
	defer logutil.Flush()

	cobra.CheckErr(cli.Root.Execute())
}

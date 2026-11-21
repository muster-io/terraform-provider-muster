// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Command terraform-provider-muster is the Terraform and OpenTofu provider of Muster.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/muster-io/terraform-provider-muster/internal/provider"
)

// version is set by the release build with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider for a debugger such as delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address:         "registry.terraform.io/muster-io/muster",
		Debug:           debug,
		ProtocolVersion: 6,
	})
	if err != nil {
		log.Fatal(err)
	}
}

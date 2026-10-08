// Command module is the Viam module entrypoint serving the mux-scale models.
package main

import (
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"github.com/viam-labs/mux-scale/muxscale"
)

func main() {
	module.ModularMain(resource.APIModel{API: sensor.API, Model: muxscale.Model})
}

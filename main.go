package main

import (
	"github.com/hashicorp/terraform-plugin-sdk/plugin"
	"github.com/moto-taka/terraform-provider-mysql/mysql"
)

func main() {
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: mysql.Provider})
}

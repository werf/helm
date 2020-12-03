package helm_v3

import (
	"io"

	"helm.sh/helm/v3/internal/experimental/registry"
)

var (
	Settings = settings

	LoadReleasesInMemory = loadReleasesInMemory
	Debug                = debug
	NewRootCmd           = newRootCmd

	NewUninstallCmd  = newUninstallCmd
	NewDependencyCmd = newDependencyCmd
	NewGetCmd        = newGetCmd
	NewHistoryCmd    = newHistoryCmd
	NewLintCmd       = newLintCmd
	NewListCmd       = newListCmd
	NewRepoCmd       = newRepoCmd
	NewRollbackCmd   = newRollbackCmd
	NewCreateCmd     = newCreateCmd
	NewEnvCmd        = newEnvCmd
	NewPackageCmd    = newPackageCmd
	NewPluginCmd     = newPluginCmd
	NewPullCmd       = newPullCmd
	NewSearchCmd     = newSearchCmd
	NewStatusCmd     = newStatusCmd
	NewTestCmd       = newReleaseTestCmd
	NewVerifyCmd     = newVerifyCmd
	NewVersionCmd    = newVersionCmd
	NewChartCmd      = newChartCmd
	NewShowCmd       = newShowCmd

	// NOTE: following commands has additional options param and defined in corresponding command files
	//NewTemplateCmd   = newTemplateCmd
	//NewInstallCmd    = newInstallCmd
	//NewUpgradeCmd    = newUpgradeCmd

	LoadPlugins = loadPlugins
)

func NewRegistryClient(debug bool, out io.Writer) (*registry.Client, error) {
	return registry.NewClient(
		registry.ClientOptDebug(debug),
		registry.ClientOptWriter(out),
	)
}

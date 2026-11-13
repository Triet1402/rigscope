package gpu

import "os"

// The helper fixture override lives only in test builds. Production never
// reads this variable or any fixture path: pci_helper.go leaves the child root
// empty and enforces the host /sys/devices boundary.
const helperRootEnv = "RIGSCOPE_GPU_PCI_HELPER_ROOT"

func init() {
	helperChildRoot = func() string { return os.Getenv(helperRootEnv) }
	helperLaunchEnv = func(resourceRoot string) []string {
		if resourceRoot == "" {
			return nil
		}
		return []string{helperRootEnv + "=" + resourceRoot}
	}
}

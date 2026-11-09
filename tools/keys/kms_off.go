//go:build !kms_aws || !kms_gcp

package keys

import "fmt"

func notBuilt(envName, tag string) error {
	return fmt.Errorf("%s is set but this build has no support for it: rebuild with -tags %s (the release image does not include it unless built with that tag)", envName, tag)
}

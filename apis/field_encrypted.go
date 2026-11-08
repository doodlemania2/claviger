package apis

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #75).

var invalidSortErrRegex = regexp.MustCompile(`invalid sort field "([^"]+)"`)

var encryptedFieldFilterErrRegex = regexp.MustCompile(`Encrypted fields cannot be used in filters, sorts or API rules: "([^"]+)" is an encrypted field`)

// encryptedFieldFilterError turns the resolver error raised for a filter or
// sort that references an encryptedText field into a 400 with a clear message.
//
// The search package flattens resolver errors into strings, so the typed
// error does not survive and the message has to be recognized by its text.
// It returns nil (an untyped nil, safe for firstApiError) for any other error.
func encryptedFieldFilterError(err error, collection *core.Collection) error {
	if err == nil {
		return nil
	}

	name := ""
	if m := encryptedFieldFilterErrRegex.FindStringSubmatch(err.Error()); m != nil {
		name = m[1]
	} else if m := invalidSortErrRegex.FindStringSubmatch(err.Error()); m != nil && collection != nil {
		// the sort expression builder drops the resolver error
		if f := collection.Fields.GetByName(strings.TrimLeft(m[1], "+-")); f != nil && core.IsEncryptedFieldType(f.Type()) {
			name = f.GetName()
		}
	}

	if name == "" {
		return nil
	}

	return router.NewBadRequestError(
		fmt.Sprintf("Encrypted fields cannot be used in filters, sorts or API rules: %q is an encrypted field.", name),
		nil,
	)
}

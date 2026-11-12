package core

import (
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
)

// Fork-local (Claviger #81, 81-4): a person's own view of their credentials.

// MyAccessCoordinator is a coordinator a volunteer can ask about a credential.
// Only the display name is given: no email, no id.
type MyAccessCoordinator struct {
	Name string `json:"name"`
}

// MyAccessType is the standing of the signed-in person for one credential type.
type MyAccessType struct {
	Key      string     `json:"key"`
	Name     string     `json:"name"`
	Standing string     `json:"standing"`
	Expires  *time.Time `json:"expires,omitempty"`
	DaysLeft *int       `json:"daysLeft,omitempty"`
	// IssuedByOrg is the name of the org that issued the best credential.
	IssuedByOrg string `json:"issuedByOrg,omitempty"`
}

// MyAccessOrg is the standing of the signed-in person in one organization.
type MyAccessOrg struct {
	Org          string                `json:"org"`
	Name         string                `json:"name"`
	Types        []MyAccessType        `json:"types"`
	Coordinators []MyAccessCoordinator `json:"coordinators"`
}

// MyCredentialStatus tells a person, for each org they actively belong to and
// each active credential type that applies there, whether they are cleared,
// expiring, pending, lapsed, revoked or have none recorded. It reads only the
// person's own rows. The reason of a revocation is never part of it.
func MyCredentialStatus(app App, userId string, now time.Time, expiringDays int) ([]MyAccessOrg, error) {
	if expiringDays <= 0 {
		expiringDays = DefaultCredentialExpiringDays
	}

	memberships, err := ActiveOrgMemberships(app, userId)
	if err != nil {
		return nil, err
	}

	mine, err := app.FindAllRecords(CollectionNameCredentials, dbx.HashExp{"user": userId})
	if err != nil {
		return nil, err
	}

	out := []MyAccessOrg{}
	orgNames := map[string]string{}
	nameOf := func(id string) string {
		if n, ok := orgNames[id]; ok {
			return n
		}

		if o, err := app.FindRecordById(CollectionNameOrgs, id); err == nil {
			orgNames[id] = o.GetString("name")
		} else {
			orgNames[id] = ""
		}

		return orgNames[id]
	}

	for _, m := range memberships {
		org, err := app.FindRecordById(CollectionNameOrgs, m.GetString("org"))
		if err != nil {
			continue
		}

		typesList, err := CredentialTypesFor(app, org.Id)
		if err != nil {
			return nil, err
		}

		chain := map[string]bool{}
		for _, id := range orgChain(app, org.Id) {
			chain[id] = true
		}

		item := MyAccessOrg{Org: org.Id, Name: org.GetString("name"), Types: []MyAccessType{}, Coordinators: []MyAccessCoordinator{}}

		for _, typ := range typesList {
			if typ.GetString("status") != CredentialTypeActive {
				continue
			}

			var creds []*Record

			for _, c := range mine {
				if c.GetString("type") == typ.GetString("key") && chain[c.GetString("org")] {
					creds = append(creds, c)
				}
			}

			standing, best := CredentialStanding(creds, now, expiringDays)
			row := MyAccessType{Key: typ.GetString("key"), Name: typ.GetString("name"), Standing: standing}

			if best != nil {
				exp := best.GetDateTime("expires").Time()
				days := int(exp.Sub(now).Hours() / 24)
				row.Expires, row.DaysLeft, row.IssuedByOrg = &exp, &days, nameOf(best.GetString("org"))
			}

			item.Types = append(item.Types, row)
		}

		if coords, err := OrgCoordinators(app, org.Id); err == nil {
			userCol, _ := OrgUserCollection(app)
			for _, u := range coords {
				if userCol != nil && userCol.Fields.GetByName("name") != nil && u.GetString("name") != "" {
					item.Coordinators = append(item.Coordinators, MyAccessCoordinator{Name: u.GetString("name")})
				}
			}
		}

		out = append(out, item)
	}

	return out, nil
}

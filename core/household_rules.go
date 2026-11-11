package core

import "fmt"

// Fork-local (Claviger #80, 80-2): rule snippets for households.
//
// There is no `isGuardianOf()` helper (the rule resolver is upstream's and
// stays untouched). The rules are plain filter syntax that walks the back
// relation `_householdMembers_via_household`, the form that
// docs/specs/80-88-congregations.md 2.3 ran.
//
// Back relations are multi-valued, so every comparison uses `?=`. Conditions
// that share the path prefix are evaluated on the SAME member row, which is
// what makes "I am an active guardian of this household" mean one row.
//
// Apps' own collections that describe a person carry a relation `member` to
// _householdMembers and a copy `household` (a relation to _households). The
// copy keeps the rules to one hop. BindHouseholdCopy keeps it in step.

// householdBackRel is the back relation from a household to its member rows.
func householdBackRel(field string) string {
	if field == "" {
		return "_householdMembers_via_household"
	}

	return field + "._householdMembers_via_household"
}

// HouseholdRuleOpts names the relations of a collection that describes a person.
type HouseholdRuleOpts struct {
	// Household is the relation to _households (the copy). Default "household".
	Household string
	// Member is the relation to _householdMembers. Default "member". Set it to
	// "-" for the _householdMembers collection itself, where the row is the person.
	Member string
}

func (o HouseholdRuleOpts) household() string {
	if o.Household == "" {
		return "household"
	}

	return o.Household
}

func (o HouseholdRuleOpts) member() string {
	if o.Member == "" {
		return "member"
	}

	return o.Member
}

// path returns the prefix for the person's attributes.
func (o HouseholdRuleOpts) person(attr string) string {
	if o.member() == "-" {
		return attr
	}

	return o.member() + "." + attr
}

// HouseholdMemberRule is the condition "the authenticated user is an active
// member of the household" (a login on one of its active member rows).
func HouseholdMemberRule(field string) string {
	p := householdBackRel(field)

	return fmt.Sprintf(`@request.auth.id != "" && %s.user ?= @request.auth.id && %s.status ?= "active"`, p, p)
}

// HouseholdGuardianRule is rule M of the spec: "I am an active guardian in
// this record's household". Minors cannot be guardians (the model hook
// refuses it), so the flag alone is enough.
func HouseholdGuardianRule(field string) string {
	p := householdBackRel(field)

	return fmt.Sprintf(`@request.auth.id != "" && %s.user ?= @request.auth.id && %s.guardian ?= true && %s.status ?= "active"`, p, p, p)
}

// HouseholdPersonReadRule lets household members read a record about a person,
// and the person themselves when they have a login.
func HouseholdPersonReadRule(opts HouseholdRuleOpts) string {
	return fmt.Sprintf(`@request.auth.id != "" && ((%s) || %s = @request.auth.id)`,
		HouseholdMemberRule(opts.household()), opts.person("user"))
}

// HouseholdGuardianWriteRule lets an active guardian write a record about a
// MINOR of the household. It ends at graduation (isMinor turns false) and when
// the guardian is removed, with no job: the rule reads live. The member must
// belong to the household named on the record.
func HouseholdGuardianWriteRule(opts HouseholdRuleOpts) string {
	rule := fmt.Sprintf(`@request.auth.id != "" && %s && %s = true`, HouseholdGuardianRule(opts.household()), opts.person("isMinor"))

	if opts.member() != "-" {
		rule += fmt.Sprintf(` && %s = %s`, opts.person("household"), opts.household())
	}

	return rule
}

// HouseholdStaffRule is the condition "the user is staff of the org at field":
// an owner or admin, or a member tagged "registrar".
func HouseholdStaffRule(orgField string) string {
	tag, _ := OrgTagRule(orgField, HouseholdRegistrarTag)

	return fmt.Sprintf(`(%s) || (%s && %s._orgMembers_via_org.role ?= "%s")`,
		OrgMemberRule(orgField, OrgRoleOwner, OrgRoleAdmin), tag, orgField, OrgRoleMember)
}

// HouseholdRuleSet is the preset for an app collection about people: household
// members (and the person) read; an active guardian writes while the person is
// a minor. Delete follows write.
func HouseholdRuleSet(opts HouseholdRuleOpts) OrgRuleSet {
	read := HouseholdPersonReadRule(opts)
	write := HouseholdGuardianWriteRule(opts)

	return OrgRuleSet{List: &read, View: &read, Create: &write, Update: &write, Delete: &write}
}

// householdRules returns the list and view rules of the two system collections:
// the people of the household (active rows with a login), and the staff of the
// household's org. A person with a login also reads their own row.
func householdRules() (households, members string) {
	households = fmt.Sprintf(`@request.auth.id != "" && ((%s) || (%s))`,
		HouseholdMemberRule(""), HouseholdStaffRule("org"))
	members = fmt.Sprintf(`@request.auth.id != "" && (user = @request.auth.id || (%s) || (%s))`,
		HouseholdMemberRule("household"), HouseholdStaffRule("household.org"))

	return households, members
}

// HouseholdRules returns the list/view rules of _households and
// _householdMembers (used by the migration and the docs).
func HouseholdRules() (households, members string) { return householdRules() }

// BindHouseholdCopy keeps the `household` copy of an app collection in step:
// on every validation it is set to the household of the `member`. Without it a
// guardian of one household could write a record naming their household and
// another household's child. opts.Member must name a relation to
// _householdMembers (not "-").
func BindHouseholdCopy(app App, collection string, opts HouseholdRuleOpts) {
	app.OnRecordValidate(collection).BindFunc(func(e *RecordEvent) error {
		if id := e.Record.GetString(opts.member()); id != "" {
			if m, err := e.App.FindRecordById(CollectionNameHouseholdMembers, id); err == nil {
				e.Record.Set(opts.household(), m.GetString("household"))
			}
		}

		return e.Next()
	})
}

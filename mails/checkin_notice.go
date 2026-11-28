package mails

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #82): the check-in notices. They carry no child's name,
// no code and no reason: the coordinators read those where they decide, in the
// org's check-in log.
//
//	email.checkinAlert.locked.*          a session was locked after five wrong codes
//	email.checkinAlert.do_not_release.*  a do-not-release flag stopped a release
//	email.checkinAlert.override.*        a supervisor released a child against a mismatch
//	email.checkinOverride.*              the same override, to the household's guardians

func init() {
	core.CheckinNotifier = SendCheckinNotice
}

// SendCheckinNotice emails the recipients of the notice and returns how many it reached.
func SendCheckinNotice(app core.App, n *core.CheckinNotice) (int, error) {
	prefix := "checkinAlert." + n.Kind

	if n.Audience == "guardians" {
		prefix = "checkinOverride"
	}

	return sendPastoralNotice(app, prefix, n.Org, "", n.Recipients)
}

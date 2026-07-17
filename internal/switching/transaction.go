// transaction.go — SwitchTransaction: the rollback ledger for the normal switch
// path (spec 02§8.2, models.SwitchTransaction). Completed steps are replayed in
// REVERSE order on failure: restore credentials, then config text (+chmod 0600),
// then the sequence's activeAccountNumber. Each step is best-effort; rollback
// reports overall success so the caller can pick the right SwitchError message.
package switching

import "git.dpemmons.com/dpemmons/cswap/internal/store"

// switchTransaction captures the pre-switch state and the steps that completed,
// so a mid-switch failure can be undone.
type switchTransaction struct {
	originalCredentials string
	originalConfig      string
	originalAccountNum  string
	originalEmail       string
	completedSteps      []string
}

// recordStep marks a completed step (credentials_written / config_written /
// sequence_updated).
func (t *switchTransaction) recordStep(step string) {
	t.completedSteps = append(t.completedSteps, step)
}

// rollback replays every completed step in reverse order, returning true only
// when every step restored cleanly (spec 02§8.2). Failures are logged and
// continue; the overall bool drives the caller's rolled-back vs
// rollback-also-failed message.
func (t *switchTransaction) rollback(s *store.Store) bool {
	success := true
	for i := len(t.completedSteps) - 1; i >= 0; i-- {
		step := t.completedSteps[i]
		if err := t.rollbackStep(s, step); err != nil {
			if s.Log != nil {
				s.Log.Errorf("Failed to rollback step %s: %v", step, err)
			}
			success = false
			continue
		}
		if s.Log != nil {
			s.Log.Infof("Rolled back step: %s", step)
		}
	}
	return success
}

func (t *switchTransaction) rollbackStep(s *store.Store, step string) error {
	switch step {
	case "credentials_written":
		return s.Creds.WriteActive(t.originalCredentials)
	case "config_written":
		return writeConfigText(t.originalConfig)
	case "sequence_updated":
		data, err := s.ReadSequence()
		if err != nil || data == nil {
			return err
		}
		n, _ := parseInt(t.originalAccountNum)
		data.ActiveAccountNumber = &n
		data.LastUpdated = storeTimestamp(s)
		return s.WriteSequence(data)
	}
	return nil
}

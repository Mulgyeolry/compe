package model

func CompositeStatus(registration RegistrationPhase, competition CompetitionPhase) Status {
	switch competition {
	case CompetitionFinished:
		return StatusFinished
	case CompetitionOngoing:
		return StatusOngoing
	case CompetitionUpcoming:
		return StatusUpcoming
	}
	switch registration {
	case RegistrationOpen:
		return StatusRegistrationOpen
	case RegistrationClosed:
		return StatusRegistrationClosed
	case RegistrationPreview:
		return StatusPreview
	default:
		return StatusUnknown
	}
}

func PhasesForLegacyStatus(status Status) (RegistrationPhase, CompetitionPhase) {
	switch status {
	case StatusPreview:
		return RegistrationPreview, CompetitionUnknown
	case StatusRegistrationOpen:
		return RegistrationOpen, CompetitionUnknown
	case StatusRegistrationClosed:
		return RegistrationClosed, CompetitionUnknown
	case StatusUpcoming:
		return RegistrationClosed, CompetitionUpcoming
	case StatusOngoing:
		return RegistrationClosed, CompetitionOngoing
	case StatusFinished:
		return RegistrationClosed, CompetitionFinished
	default:
		return RegistrationUnknown, CompetitionUnknown
	}
}

// NormalizeLifecycle keeps legacy and phase-aware callers interoperable while
// the public UI continues to render one compact status label.
func NormalizeLifecycle(value *Competition) {
	if value == nil {
		return
	}
	if value.RegistrationPhase == "" {
		value.RegistrationPhase = RegistrationUnknown
	}
	if value.CompetitionPhase == "" {
		value.CompetitionPhase = CompetitionUnknown
	}
	if value.RegistrationPhase == RegistrationUnknown && value.CompetitionPhase == CompetitionUnknown && value.Status != "" && value.Status != StatusUnknown {
		value.RegistrationPhase, value.CompetitionPhase = PhasesForLegacyStatus(value.Status)
	}
	value.Status = CompositeStatus(value.RegistrationPhase, value.CompetitionPhase)
}

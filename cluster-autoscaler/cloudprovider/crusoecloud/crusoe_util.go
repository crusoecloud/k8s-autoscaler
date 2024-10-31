package crusoecloud

type opStatus string

const (
	opSucceeded  opStatus = "SUCCEEDED"
	opInProgress opStatus = "IN_PROGRESS"
	opFailed     opStatus = "FAILED"
)

package protocol

import "regexp"

var credentialName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// ValidCredentialName 让 manifest 与保险库对条目名两段采用同一边界，斜线只由内核添加。
func ValidCredentialName(s string) bool { return credentialName.MatchString(s) }

package vm

import "regexp"

var runnerReadyLine = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: Listening for Jobs$`)

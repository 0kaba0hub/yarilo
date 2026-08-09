package mimesalvage

import "errors"

// ErrUnsalvageable means the input has no header line that could be one and no
// header/body boundary either: there is no message here to repair, and saying
// so is more useful than returning an empty one.
var ErrUnsalvageable = errors.New("mimesalvage: nothing that could be a message")

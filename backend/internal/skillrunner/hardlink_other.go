//go:build !unix

package skillrunner

import "os"

// hardLinkCount cannot be determined on this platform, so every file reads as
// singly-linked. The rest of the validation still applies.
func hardLinkCount(os.FileInfo) uint64 { return 1 }

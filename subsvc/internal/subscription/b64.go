package subscription

import "encoding/base64"

// encodings lists the Base64 variants seen in the wild once padding has been
// stripped: standard and URL-safe alphabets.
var encodings = []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding}

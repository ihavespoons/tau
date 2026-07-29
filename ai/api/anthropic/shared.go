package anthropic

// The message transform, deferred-tool split, and unicode sanitization now
// live in apishared, because openaichat needs the same behavior. These aliases
// keep this package's call sites reading the way they did before the move.

import "github.com/ihavespoons/tau/ai/api/apishared"

var (
	sanitizeSurrogates = apishared.SanitizeSurrogates
	transformMessages  = apishared.TransformMessages
	splitDeferredTools = apishared.SplitDeferredTools
	trimSpace          = apishared.TrimSpace
)

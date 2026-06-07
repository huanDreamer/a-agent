package llm

import "errors"

// stdErrorsAs is a thin re-export so test files don't need to import "errors"
// directly when checking wrapped LLMError values.
func stdErrorsAs(err error, target any) bool { return errors.As(err, target) }

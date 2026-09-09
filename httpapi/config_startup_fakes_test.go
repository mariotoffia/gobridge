package httpapi

type startupHTTPError int

func (e startupHTTPError) Error() string       { return "backend response" }
func (e startupHTTPError) HTTPStatusCode() int { return int(e) }

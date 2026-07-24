package adl

import "errors"

func stdErrorsAs(err error, target any) bool {
	return errors.As(err, target)
}

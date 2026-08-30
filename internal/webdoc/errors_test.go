package webdoc_test

import "errors"

func errorAs(err error, target any) bool { return errors.As(err, target) }

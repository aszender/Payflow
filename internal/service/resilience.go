package service

func Retry(f func() error) error {
	return f()
}

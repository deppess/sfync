module github.com/deppess/sfync/testenv

go 1.27

require (
	github.com/deppess/sfync v0.0.0
	github.com/fclairamb/ftpserverlib v0.32.4
	github.com/pkg/sftp v1.13.12
	github.com/spf13/afero v1.15.0
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/bmatcuk/doublestar/v4 v4.10.4 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/jlaffaye/ftp v0.2.4 // indirect
	github.com/kr/fs v0.1.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/deppess/sfync => ../

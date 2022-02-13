package main

import (
	_ "github.com/mattn/go-sqlite3"

	"database/sql"
	"errors"
	"os"
	"path"
	"runtime"
)

type Sqlite struct {
	*sql.DB

	Path   string
	Exists bool
}

func NewSqlite(basename string) (*Sqlite, error) {
	var dbpath string
	if _, file, _, ok := runtime.Caller(1); !ok {
		return nil, errors.New("runtime.Caller() failed")
	} else {
		dbpath = path.Join(path.Dir(file), basename)
	}

	var exists bool
	if _, err := os.Stat(dbpath); err != nil {
		if os.IsNotExist(err) {
			exists = false
		} else {
			return nil, err
		}
	} else {
		exists = true
	}

	db, err := sql.Open("sqlite3", dbpath)
	if err != nil {
		return nil, err
	}

	rv := Sqlite{
		    DB: db,
		  Path: dbpath,
		Exists: exists,
	}
	return &rv, nil
}

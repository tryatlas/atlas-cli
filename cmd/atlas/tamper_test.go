package main

import "database/sql"

func tamper(path string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE revisions SET content = 'edited by hand' WHERE rev = 1`)
	return err
}

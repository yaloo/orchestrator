/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"fmt"
	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
	"github.com/outbrain/orchestrator/config"
	"github.com/outbrain/orchestrator/db"
)

// WriteResolvedHostname stores a hostname and the resolved hostname to backend database
func WriteResolvedHostname(hostname string, resolvedHostname string) error {
	writeFunc := func() error {
		db, err := db.OpenOrchestrator()
		if err != nil {
			return log.Errore(err)
		}

		_, err = sqlutils.Exec(db, `
			insert into  
					hostname_resolve (hostname, resolved_hostname, resolved_timestamp)
				values
					(?, ?, NOW())
				on duplicate key update
					resolved_hostname = VALUES(resolved_hostname), 
					resolved_timestamp = VALUES(resolved_timestamp)
			`,
			hostname,
			resolvedHostname)
		if err != nil {
			return log.Errore(err)
		}
		log.Debugf("WriteResolvedHostname: resolved %s to %s", hostname, resolvedHostname)

		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// ReadResolvedHostname returns the resolved hostname given a hostname, or empty if not exists
func ReadResolvedHostname(hostname string) (string, error) {
	var resolvedHostname string = ""

	query := fmt.Sprintf(`
		select 
			resolved_hostname
		from 
			hostname_resolve
		where
			hostname = '%s'
		`, hostname)
	db, err := db.OpenOrchestrator()
	if err != nil {
		goto Cleanup
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		resolvedHostname = m.GetString("resolved_hostname")
		return nil
	})
Cleanup:

	if err != nil {
		log.Errore(err)
	}
	return resolvedHostname, err
}

func readAllHostnameResolves() ([]HostnameResolve, error) {
	res := []HostnameResolve{}
	query := fmt.Sprintf(`
		select 
			hostname, 
			resolved_hostname  
		from 
			hostname_resolve
		`)
	db, err := db.OpenOrchestrator()
	if err != nil {
		goto Cleanup
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		hostnameResolve := HostnameResolve{hostname: m.GetString("hostname"), resolvedHostname: m.GetString("resolved_hostname")}

		res = append(res, hostnameResolve)
		return nil
	})
Cleanup:

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// readUnresolvedHostname reverse-reads hostname resolve. It returns a hostname which matches given pattern and resovles to resolvedHostname,
// or, in the event no such hostname is found, the given resolvedHostname, unchanged.
func readUnresolvedHostname(hostname string) (string, error) {
	unresolvedHostname := hostname

	query := fmt.Sprintf(`
	   		select
	   			unresolved_hostname
	   		from
	   			hostname_unresolve
	   		where
	   			hostname = '%s'
	   		`, hostname)
	db, err := db.OpenOrchestrator()
	if err != nil {
		goto Cleanup
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		unresolvedHostname = m.GetString("unresolved_hostname")
		return nil
	})
Cleanup:

	if err != nil {
		log.Errore(err)
	}
	return unresolvedHostname, err
}

// ForgetExpiredHostnameResolves
func ForgetExpiredHostnameResolves() error {
	db, err := db.OpenOrchestrator()
	if err != nil {
		return log.Errore(err)
	}

	_, err = sqlutils.Exec(db, `
			delete 
				from hostname_resolve 
			where 
				resolved_timestamp < NOW() - interval (? * 2) minute`,
		config.Config.ExpiryHostnameResolvesMinutes,
	)
	return err
}

// DeleteInvalidHostnameResolves removes invalid resolves. At this time these are:
// - infinite loop resolves (A->B and B->A), remove earlier mapping
func DeleteInvalidHostnameResolves() error {
	var invalidHostnames []string
	query := `
		select 
		    early.hostname
		  from 
		    hostname_resolve as latest 
		    join hostname_resolve early on (latest.resolved_hostname = early.hostname and latest.hostname = early.resolved_hostname) 
		  where 
		    latest.hostname != latest.resolved_hostname 
		    and latest.resolved_timestamp > early.resolved_timestamp
	   	`
	db, err := db.OpenOrchestrator()
	if err != nil {
		return err
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		invalidHostnames = append(invalidHostnames, m.GetString("hostname"))
		return nil
	})
	if err != nil {
		return err
	}

	for _, invalidHostname := range invalidHostnames {
		_, err = sqlutils.Exec(db, `
			delete 
				from hostname_resolve 
			where 
				hostname = ?`,
			invalidHostname,
		)
		if err != nil {
			log.Errore(err)
		}
	}
	return err
}

// deleteHostnameResolves compeltely erases the database cache
func deleteHostnameResolves() error {
	db, err := db.OpenOrchestrator()
	if err != nil {
		return log.Errore(err)
	}

	_, err = sqlutils.Exec(db, `
			delete 
				from hostname_resolve`,
	)
	return err
}

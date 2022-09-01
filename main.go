package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/cbroglie/mustache"
	flags "github.com/jessevdk/go-flags"
	"golang.org/x/term"
	pg "gopkg.in/pg.v4"
	yaml "gopkg.in/yaml.v2"
)

const (
	BEGIN_DUMP = `
--
-- PostgreSQL database dump
--

BEGIN;

SET statement_timeout = 0;
SET lock_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET client_min_messages = warning;

SET search_path = public, pg_catalog;

`

	END_DUMP = `
COMMIT;

--
-- PostgreSQL database dump complete
--
`

	BEGIN_TABLE_DUMP = `
--
-- Data for Name: %s; Type: TABLE DATA
--

COPY %s (%s) FROM STDIN;
`

	END_TABLE_DUMP = `\.
`

	SQL_CMD_DUMP = "\n%s;\n"
)

type Options struct {
	Host             string
	Port             int
	Username         string
	NoPasswordPrompt bool
	Password         string
	ManifestFile     string
	OutputFile       string
	Database         string
	UseTLS           bool
}

type ManifestItem struct {
	Table       string   `yaml:"table"`
	Query       string   `yaml:"query"`
	Columns     []string `yaml:"columns,flow"`
	PostActions []string `yaml:"post_actions,flow"`
}

type Manifest struct {
	Vars   map[string]string `yaml:"vars"`
	Tables []ManifestItem    `yaml:"tables"`
}

type ManifestIterator struct {
	db       *pg.DB
	manifest *Manifest
	todo     map[string]ManifestItem
	done     map[string]ManifestItem
	stack    []string
}

func NewManifestIterator(db *pg.DB, manifest *Manifest) *ManifestIterator {
	m := ManifestIterator{
		db,
		manifest,
		make(map[string]ManifestItem),
		make(map[string]ManifestItem),
		make([]string, 0),
	}

	for _, item := range m.manifest.Tables {
		m.stack = append(m.stack, item.Table)
		m.todo[item.Table] = item
	}

	return &m
}

func (m *ManifestIterator) Next() (*ManifestItem, error) {
	if len(m.stack) == 0 {
		return nil, nil
	}

	table := m.stack[0]
	m.stack = m.stack[1:]

	if _, ok := m.todo[table]; !ok {
		return m.Next()
	}

	deps, err := getTableDeps(m.db, table)
	if err != nil {
		return nil, err
	}

	todoDeps := make([]string, 0)
	for _, dep := range deps {
		_, isTodo := m.todo[dep]
		_, isDone := m.done[dep]
		if !isTodo && !isDone {
			// A new dependency table not present in the manifest file was
			// found, create a default entry for it
			m.todo[dep] = ManifestItem{Table: dep}
		}
		if _, ok := m.todo[dep]; ok && table != dep {
			todoDeps = append(todoDeps, dep)
		}
	}

	if len(todoDeps) > 0 {
		m.stack = append(todoDeps, append([]string{table}, m.stack...)...)
		return m.Next()
	}

	result := m.todo[table]
	m.done[table] = m.todo[table]
	delete(m.todo, table)

	return &result, nil
}

func parseArgs() (*Options, error) {
	var opts struct {
		Host             string `short:"h" long:"host" default:"/tmp" default-mask:"local socket" env:"PGHOST" description:"Database server host or socket directory"`
		Port             string `short:"p" long:"port" default:"5432" env:"PGPORT" description:"Database server port"`
		Username         string `short:"U" long:"username" default-mask:"current user" env:"PGUSER" description:"Database user name"`
		NoPasswordPrompt bool   `short:"w" long:"no-password" description:"Don't prompt for password"`
		ManifestFile     string `short:"f" long:"manifest-file" description:"Path to manifest file"`
		OutputFile       string `short:"o" long:"output-file" description:"Path to the output file"`
		UseTLS           bool   `short:"s" long:"tls" description:"Use SSL/TLS database connection"`
		Help             bool   `long:"help" description:"Show help"`
	}

	parser := flags.NewParser(&opts, flags.None)
	parser.Usage = "[options] database"

	args, err := parser.Parse()
	if err != nil {
		parser.WriteHelp(os.Stderr)
		return nil, err
	}

	if opts.Help {
		parser.WriteHelp(os.Stdout)
		os.Exit(0)
	}

	// Manifest file
	if opts.ManifestFile == "" {
		parser.WriteHelp(os.Stderr)
		return nil, fmt.Errorf("required flag `-f, --manifest-file` not specified")
	}

	// Username
	if opts.Username == "" {
		currentUser, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("failed to get current user")
		}
		opts.Username = currentUser.Username
	}

	// Port
	port, err := strconv.Atoi(opts.Port)
	if err != nil {
		parser.WriteHelp(os.Stderr)
		return nil, fmt.Errorf("port must be a number 0-65535")
	}

	// Database
	Database := ""
	if len(args) == 0 {
		Database = os.Getenv("PGDATABASE")
	} else if len(args) == 1 {
		Database = args[0]
	} else if len(args) > 1 {
		parser.WriteHelp(os.Stderr)
		return nil, fmt.Errorf("only one database may be specified at a time")
	}

	// Password
	Password := os.Getenv("PGPASSWORD")

	return &Options{
		Host:             opts.Host,
		Port:             port,
		Username:         opts.Username,
		NoPasswordPrompt: opts.NoPasswordPrompt,
		Password:         Password,
		ManifestFile:     opts.ManifestFile,
		OutputFile:       opts.OutputFile,
		UseTLS:           opts.UseTLS,
		Database:         Database,
	}, nil
}

func connectDB(opts *pg.Options) (*pg.DB, error) {
	db := pg.Connect(opts)
	var model []struct {
		X string
	}
	_, err := db.Query(&model, `SELECT 1 AS x`)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func beginDump(w io.Writer) {
	fmt.Fprintf(w, BEGIN_DUMP)
}

func endDump(w io.Writer) {
	fmt.Fprintf(w, END_DUMP)
}

func beginTable(w io.Writer, table string, columns []string) {
	quoted := make([]string, 0)
	for _, v := range columns {
		quoted = append(quoted, strconv.Quote(v))
	}
	colstr := strings.Join(quoted, ", ")
	fmt.Fprintf(w, BEGIN_TABLE_DUMP, table, table, colstr)
}

func endTable(w io.Writer) {
	fmt.Fprintf(w, END_TABLE_DUMP)
}

func dumpSqlCmd(w io.Writer, v string) {
	fmt.Fprintf(w, SQL_CMD_DUMP, v)
}

func dumpTable(w io.Writer, db *pg.DB, table string) error {
	sql := fmt.Sprintf(`COPY %s TO STDOUT`, table)

	_, err := db.CopyTo(w, sql)
	if err != nil {
		return err
	}

	return nil
}

func readPassword(username string) (string, error) {
	fmt.Fprintf(os.Stderr, "Password for %s: ", username)
	password, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Print("\n")
	return string(password), err
}

func readManifest(r io.Reader) (*Manifest, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	manifest := Manifest{}
	yaml.Unmarshal(data, &manifest)

	return &manifest, nil
}

func getTableCols(db *pg.DB, table string) ([]string, error) {
	var model []struct {
		Colname string
	}
	sql := `
		SELECT attname as colname
		FROM pg_catalog.pg_attribute
		WHERE
			attrelid = ?::regclass
			AND attnum > 0
			AND attisdropped = FALSE
			ORDER BY attnum
	`
	_, err := db.Query(&model, sql, table)
	if err != nil {
		return nil, err
	}

	var cols = make([]string, 0)
	for _, v := range model {
		cols = append(cols, v.Colname)
	}

	return cols, nil
}

func getTableDeps(db *pg.DB, table string) ([]string, error) {
	var model []struct {
		Tablename string
	}
	sql := `
		SELECT confrelid::regclass AS tablename
		FROM pg_catalog.pg_constraint
		WHERE
			conrelid = ?::regclass
			AND contype = 'f'
	`
	_, err := db.Query(&model, sql, table)
	if err != nil {
		return nil, err
	}

	var tables = make([]string, 0)
	for _, v := range model {
		tables = append(tables, v.Tablename)
	}

	return tables, nil
}

func makeDump(db *pg.DB, manifest *Manifest, w io.Writer) error {

	beginDump(w)

	iterator := NewManifestIterator(db, manifest)
	for {
		v, err := iterator.Next()
		if err != nil {
			return err
		}
		if v == nil {
			break
		}

		cols := v.Columns
		if len(cols) == 0 {
			cols, err = getTableCols(db, v.Table)
			if err != nil {
				return err
			}
		}

		beginTable(w, v.Table, cols)
		if v.Query == "" {
			err := dumpTable(w, db, v.Table)
			if err != nil {
				return err
			}
		} else {
			query, err := mustache.Render(v.Query, manifest.Vars)
			if err != nil {
				return err
			}

			err = dumpTable(w, db, fmt.Sprintf("(%s)", query))
			if err != nil {
				return err
			}
		}

		endTable(w)

		for _, sql := range v.PostActions {
			dumpSqlCmd(w, sql)
		}
	}

	endDump(w)

	return nil
}

func main() {
	// Parse command-line arguments
	opts, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Open manifest file
	manifestFile, err := os.Open(opts.ManifestFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Read manifest
	manifest, err := readManifest(manifestFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Open output file
	output := os.Stdout
	if opts.OutputFile != "" {
		output, err = os.OpenFile(opts.OutputFile, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0666)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Connect to the DB
	db, err := connectDB(&pg.Options{
		Addr:     fmt.Sprintf("%s:%d", opts.Host, opts.Port),
		Database: opts.Database,
		SSL:      opts.UseTLS,
		User:     opts.Username,
		Password: opts.Password,
	})
	if err != nil {
		password := opts.Password
		if !opts.NoPasswordPrompt {
			// Read database password from the terminal
			password, err = readPassword(opts.Username)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}

		// Try again, this time with password
		db, err = connectDB(&pg.Options{
			Addr:     fmt.Sprintf("%s:%d", opts.Host, opts.Port),
			Database: opts.Database,
			SSL:      opts.UseTLS,
			User:     opts.Username,
			Password: password,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Make the dump
	err = makeDump(db, manifest, output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

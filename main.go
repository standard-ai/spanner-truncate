//
// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// spanner-deleter is a tool to delete all rows from the tables in a Cloud Spanner database without deleting tables themselves.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/gosuri/uiprogress"
	"github.com/jessevdk/go-flags"
)

type options struct {
	ProjectID  string `short:"p" long:"project" description:"(required) GCP Project ID."`
	InstanceID string `short:"i" long:"instance" description:"(required) Cloud Spanner Instance ID."`
	DatabaseID string `short:"d" long:"database" description:"(required) Cloud Spanner Database ID."`
	Quiet      bool   `short:"q" long:"quiet" description:"Disable all interactive prompts."`
	Tables     string `short:"t" long:"tables" description:"Comma separated table names to be truncated. Default to truncate all tables if not specified."`
	Column     string `short:"c" long:"column" description:"Column used to perform greater than and less than filter"`
	Values     string `short:"v" long:"values" description:"Comma separated values for column to match on"`
	Lower      string `short:"l" long:"lower" description:"Lower bound comparison"`
	Upper      string `short:"u" long:"upper" description:"Upper bound comparison"`
	Priority   int32  `long:"priority" description:"Priority [0,3] described here https://pkg.go.dev/google.golang.org/genproto/googleapis/spanner/v1#RequestOptions_Priority"`
	Timeout    int    `short:"o" long:"timeout" description:"Timeout in days"`
}

const maxTimeout = time.Hour * 24 * 1

func main() {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		exitf("Invalid options: %s\n", err)
	}

	if opts.ProjectID == "" || opts.InstanceID == "" || opts.DatabaseID == "" {
		exitf("Missing options: -p, -i, -d are required.\n")
	}

	var targetTables []string
	if opts.Tables != "" {
		targetTables = strings.Split(opts.Tables, ",")
	}

	var columnValues []string
	if opts.Values != "" {
		columnValues = strings.Split(opts.Values, ",")
	}

	timeout := maxTimeout
	if opts.Timeout > 0 {
		timeout = time.Hour * 24 * time.Duration(opts.Timeout)
	}

	if opts.Priority < 0 || opts.Priority > 3 {
		exitf("ERROR: priority must be in [0, 3], see https://pkg.go.dev/google.golang.org/genproto/googleapis/spanner/v1#RequestOptions_Priority")
	}
	out := os.Stdout
	fmt.Fprintf(out, "Running with priority %d\n", opts.Priority)
	fmt.Fprintf(out, "Creating context with timeout %s\n", timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go handleInterrupt(cancel)

	if err := run(ctx, opts.ProjectID, opts.InstanceID, opts.DatabaseID, opts.Quiet, out, targetTables, opts.Column, columnValues, opts.Lower, opts.Upper, opts.Priority); err != nil {
		exitf("ERROR: %s", err.Error())
	}
}

func run(ctx context.Context, projectID, instanceID, databaseID string, quiet bool, out io.Writer, targetTables []string, column string, columnValues []string, lower string, upper string, priority int32) error {
	database := fmt.Sprintf("projects/%s/instances/%s/databases/%s", projectID, instanceID, databaseID)

	client, err := spanner.NewClient(ctx, database)
	if err != nil {
		return fmt.Errorf("failed to create Cloud Spanner client: %v", err)
	}

	fmt.Fprintf(out, "Fetching table schema from %s\n", database)
	schemas, err := fetchTableSchemas(ctx, client, targetTables)
	if err != nil {
		return fmt.Errorf("failed to fetch table schema: %v", err)
	}
	for _, schema := range schemas {
		fmt.Fprintf(out, "%s\n", schema.tableName)
	}
	fmt.Fprintf(out, "\n")
	if !quiet {
		if !confirm(out, "Rows in these tables will be deleted. Do you want to continue?") {
			return nil
		}
	} else {
		fmt.Fprintf(out, "Rows in these tables will be deleted.\n")
	}

	coordinator := newCoordinator(schemas, client, column, columnValues, lower, upper, priority)
	for _, table := range coordinator.tables {
		fmt.Fprintf(out, "Executing: %s\n", table.deleter.getDeleteStatementString())
	}
	coordinator.start(ctx)

	// Show progress bars.
	progress := uiprogress.New()
	progress.SetOut(out)
	progress.SetRefreshInterval(time.Millisecond * 500)
	progress.Start()
	var maxNameLength int
	for _, schema := range schemas {
		if l := len(schema.tableName); l > maxNameLength {
			maxNameLength = l
		}
	}
	for _, table := range flattenTables(coordinator.tables) {
		showProgressBar(progress, table, maxNameLength)
	}

	if err := coordinator.waitCompleted(); err != nil {
		progress.Stop()
		return fmt.Errorf("failed to delete: %v", err)
	}
	// Wait for reflecting the latest progresses to progress bars.
	time.Sleep(time.Second)
	progress.Stop()

	fmt.Fprint(out, "\nDone! All rows have been deleted successfully.\n")
	return nil
}

func exitf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format, a...)
	os.Exit(1)
}

// confirm returns true if a user confirmed the message, otherwise returns false.
func confirm(out io.Writer, msg string) bool {
	fmt.Fprintf(out, "%s [Y/n] ", msg)

	s := bufio.NewScanner(os.Stdin)
	for {
		s.Scan()
		switch s.Text() {
		case "Y":
			return true
		case "n":
			return false
		default:
			fmt.Fprint(out, "Please answer Y or n: ")
		}
	}
}

func showProgressBar(progress *uiprogress.Progress, table *table, maxNameLength int) {
	bar := progress.AddBar(100)
	bar.PrependFunc(func(b *uiprogress.Bar) string {
		elapsed := int(b.TimeElapsed().Seconds())
		return fmt.Sprintf("%5ds", elapsed)
	})
	bar.PrependFunc(func(b *uiprogress.Bar) string {
		var s string
		switch table.deleter.status {
		case statusAnalyzing:
			s = "analyzing"
		case statusWaiting:
			s = "waiting  " // append space for alignment
		case statusDeleting, statusCascadeDeleting:
			s = "deleting " // append space for alignment
		case statusCompleted:
			s = "completed"
		}
		return fmt.Sprintf("%-*s%s", maxNameLength+2, table.tableName+": ", s)
	})
	bar.AppendCompleted()
	bar.AppendFunc(func(b *uiprogress.Bar) string {
		deletedRows := table.deleter.totalRows - table.deleter.remainedRows
		return fmt.Sprintf("(%s / %s)", formatNumber(deletedRows), formatNumber(table.deleter.totalRows))
	})

	// HACK: We call progressBar.Incr() to start timer in the progress bar.
	bar.Set(-1)
	bar.Incr()

	// Update progress periodically.
	go func() {
		for {
			switch table.deleter.status {
			case statusCompleted:
				// Increment the progress bar until it reaches 100
				for bar.Incr() {
				}
			case statusAnalyzing:
				// nop
			default:
				deletedRows := table.deleter.totalRows - table.deleter.remainedRows
				target := int(float32(deletedRows) / float32(table.deleter.totalRows) * 100)
				for i := bar.Current(); i < target; i++ {
					bar.Incr()
				}
			}

			time.Sleep(time.Second * 1)
		}
	}()
}

func handleInterrupt(cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	cancel()
}

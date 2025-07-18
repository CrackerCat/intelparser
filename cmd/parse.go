package cmd

import (
    "os"
    "strings"

    "github.com/helviojunior/intelparser/internal/ascii"
    "github.com/helviojunior/intelparser/internal/tools"
    "github.com/helviojunior/intelparser/pkg/log"
    "github.com/helviojunior/intelparser/pkg/runner"
    //"github.com/helviojunior/intelparser/pkg/database"
    "github.com/helviojunior/intelparser/pkg/writers"
    //"github.com/helviojunior/intelparser/pkg/readers"
    resolver "github.com/helviojunior/gopathresolver"
    //"gorm.io/gorm"
    "github.com/spf13/cobra"
)

var scanWriters = []writers.Writer{}
var scanRunner *runner.Runner
var tempFolder string

var parserCmd = &cobra.Command{
    Use:   "parse",
    Short: "Parse Leak and Intelligence Files",
    Long: ascii.LogoHelp(ascii.Markdown(`
# parse

`)),
    Example: `
   - intelparser parse intelx -p "~/Desktop/Search 2025-02-05 10_48_28.zip"
   - intelparser parse intelx -p "~/Desktop/"
   - intelparser parse intelx -p ~/Desktop/ --write-elastic --write-elasticsearch-uri "http://127.0.0.1:9200/intelparser"
`,
    PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
        var err error

        // Annoying quirk, but because I'm overriding PersistentPreRun
        // here which overrides the parent it seems.
        // So we need to explicitly call the parent's one now.
        if err = rootCmd.PersistentPreRunE(cmd, args); err != nil {
            return err
        }

        opts.Writer.GlobalDbURI = "sqlite:///"+ opts.Writer.UserPath + "/.intelparser.db"

        basePath := ""
        if opts.StoreLocalWorkspace {
            basePath = "./"
            fp, err := resolver.ResolveFullPath("./intelparser.db")
            if err != nil {
                return err
            }
            opts.Writer.GlobalDbURI = "sqlite:///"+ fp
        }

        if tempFolder, err = tools.CreateDir(tools.TempFileName(basePath, "intelparser_", "")); err != nil {
            log.Error("error creatting temp folder", "err", err)
            os.Exit(2)
        }

        if opts.Writer.NoControlDb {
            opts.Writer.GlobalDbURI = "sqlite:///"+ tools.TempFileName(tempFolder, "intelparser_", ".db")
        }

        //The first one is the general writer (global user)
        globalDbWriter, err := writers.NewDbWriter(opts.Writer.GlobalDbURI, false)
        if err != nil {
            return err
        }
        globalDbWriter.ReadOnly = opts.Writer.NoControlDb
        scanWriters = append(scanWriters, globalDbWriter)

        //The second one is the STDOut
        if opts.Logging.Silence != true && opts.Writer.None != true {
            w, err := writers.NewStdoutWriter()
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)
        }

        // Configure writers that subcommand scanners will pass to
        // a runner instance.
        if opts.Writer.Jsonl {
            w, err := writers.NewJsonWriter(opts.Writer.JsonlFile)
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)
        }

        if opts.Writer.Db {

            if strings.Contains(opts.Writer.DbURI, "sqlite://") {
                fileName := strings.Replace(opts.Writer.DbURI, "sqlite:///", "", -1)
                fileName = strings.Replace(fileName, "sqlite://", "", -1)

                fp, err := resolver.ResolveFullPath(fileName)
                if err != nil {
                    return err
                }

                opts.Writer.DbURI = "sqlite:///" + fp
            }

            w, err := writers.NewDbWriter(opts.Writer.DbURI, opts.Writer.DbDebug)
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)

            // As we have another Database, use the default one only to control database
            globalDbWriter.ControlOnly = true
        }

        if opts.Writer.Csv {
            w, err := writers.NewCsvWriter(opts.Writer.CsvFile)
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)
        }

        if opts.Writer.None {
            w, err := writers.NewNoneWriter()
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)
        }

        if opts.Writer.ELastic {
            w, err := writers.NewElasticWriter(opts.Writer.ELasticURI)
            if err != nil {
                return err
            }
            scanWriters = append(scanWriters, w)

            // As we have another Writer, use the default database only to control database
            globalDbWriter.ControlOnly = true
        }

        if len(scanWriters) == 0 {
            log.Warn("no writers have been configured. to persist probe results, add writers using --write-* flags")
        }

        //The minumin permmited threads (to prevent dead-lock)
        if opts.Parser.Threads < 2 {
            opts.Parser.Threads = 2
        }

        return nil
    },
}

func init() {
    rootCmd.AddCommand(parserCmd)

    parserCmd.PersistentFlags().IntVarP(&opts.Parser.Threads, "threads", "t", 10, "Number of concurrent threads (goroutines) to use")
    
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.NoControlDb, "disable-control-db", false, "Disable utilization of database ~/.intelparser.db.")
    parserCmd.PersistentFlags().BoolVar(&opts.StoreLocalWorkspace, "local-workspace", false, "Use execution path to store workspace files")
    
    parserCmd.PersistentFlags().IntVar(&opts.Parser.NearTextSize, "neartext-size", 50, "Defines how much data should be captured before and after the matching text segment")
    parserCmd.PersistentFlags().BoolVar(&opts.Parser.StoreNearText, "store-neartext", false, "Stores text near rule matches for context. (warning: may drastically increase storage usage!)")

    parserCmd.PersistentFlags().BoolVar(&opts.Writer.Db, "write-db", false, "Write results to a SQLite database")
    parserCmd.PersistentFlags().StringVar(&opts.Writer.DbURI, "write-db-uri", "sqlite:///intelparser.sqlite3", "The database URI to use. Supports SQLite, Postgres, and MySQL (e.g., postgres://user:pass@host:port/db)")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.DbDebug, "write-db-enable-debug", false, "Enable database query debug logging (warning: verbose!)")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.Csv, "write-csv", false, "Write results as CSV (has limited columns)")
    parserCmd.PersistentFlags().StringVar(&opts.Writer.CsvFile, "write-csv-file", "intelparser.csv", "The file to write CSV rows to")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.Jsonl, "write-jsonl", false, "Write results as JSON lines")
    parserCmd.PersistentFlags().StringVar(&opts.Writer.JsonlFile, "write-jsonl-file", "intelparser.jsonl", "The file to write JSON lines to")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.Stdout, "write-stdout", false, "Write successful results to stdout (usefull in a shell pipeline)")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.None, "write-none", false, "Use an empty writer to silence warnings")
    parserCmd.PersistentFlags().BoolVar(&opts.Writer.ELastic, "write-elastic", false, "Write results to a SQLite database")
    parserCmd.PersistentFlags().StringVar(&opts.Writer.ELasticURI, "write-elasticsearch-uri", "http://localhost:9200/intelparser", "The elastic search URI to use. (e.g., http://user:pass@host:9200/index)")

}
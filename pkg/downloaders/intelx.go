package downloaders

import (

    "errors"
    "context"
    "sync"
    "time"
    "path/filepath"
    "strings"
    "os"
    "archive/zip"
    "io"
    "encoding/csv"
    "fmt"
	"reflect"
	"strconv"

	"bufio"

	"github.com/gofrs/uuid"

    tty "golang.org/x/term"

    "github.com/helviojunior/intelparser/internal/ascii"
    "github.com/helviojunior/intelparser/internal/tools"
    "github.com/helviojunior/intelparser/pkg/database"
    "github.com/helviojunior/intelparser/pkg/ixapi"
    "github.com/helviojunior/intelparser/pkg/log"
    "gorm.io/gorm"
	//"gorm.io/gorm/clause"
)

const textSupportedSelectors = "Selector types supported:\n  * Email address\n  * Domain, including wildcards like *.example.com\n  * URL\n  * IPv4 and IPv6\n  * CIDRv4 and CIDRv6\n  * Phone Number\n  * Bitcoin address\n  * MAC address\n  * IPFS Hash\n  * UUID\n  * Simhash\n  * Credit card number\n  * IBAN\n"
var csvExludedFields = []string{"near_text"}

var byteSizes = []string{"B", "kB"}

type IntelXDownloader struct {
	Term string
	ZipFile string
	Threads int
	ProxyURL string // Proxy to use+
	Limit int 

	apiKey string
	ctx    context.Context
	dbName string
	conn          *gorm.DB
	mutex         sync.Mutex
	tempFolder string

	status *IntelXDownloaderStatus

	results chan ixapi.SearchResult
}

type IntelXDownloaderStatus struct {
	TotalFiles int
	Downloaded int
	Duplicated int
	TotalBytes int64
	StateBytes int64
	Spin string
	Step string
	Running bool
    IsTerminal bool
}

func (st *IntelXDownloaderStatus) Print() { 
	if st.IsTerminal {
		st.Spin = ascii.GetNextSpinner(st.Spin)

		fmt.Fprintf(os.Stderr, "%s\n %s %s, reg.: %d, downloaded: %d, dup.: %d, bytes: %s               \r\033[A", 
	    	"                                                                        ",
	    	ascii.ColoredSpin(st.Spin), 
	    	st.Step, 
	    	st.TotalFiles, 
	    	st.Downloaded, 
	    	st.Duplicated, 
	    	tools.HumanateBytes(uint64(st.TotalBytes + st.StateBytes), 1000, byteSizes),
	    )
	 }else{
        log.Info("STATUS", 
            "step", st.Step, "total", st.TotalFiles, "downloaded", st.Downloaded, 
            "duplicated", st.Duplicated, "bytes", tools.HumanateBytes(uint64(st.TotalBytes + st.StateBytes), 1000, byteSizes))
    }
} 

func (st *IntelXDownloaderStatus) Clear() { 
	//fmt.Fprintf(os.Stderr, "\r%s\r", 
    //        "                                                                                ",
    //    )
    ascii.ClearLine()
    ascii.ShowCursor()
}

func NewIntelXDownloader(term string, apiKey string, outZipFile string) (*IntelXDownloader, error) {
    tempFolder, err := tools.CreateDir(tools.TempFileName("", "intelparser_", ""))
    if err != nil {
        return nil, err
    }

    dbName := filepath.Join(tempFolder, "info.sqlite3")
    log.Debug("Creating info database", "path", dbName)
	c, err := database.Connection("sqlite:///"+ dbName, false, false)
	if err != nil {
		return nil, err
	}

	// run database migrations on the connection
	if err := c.AutoMigrate(
		&ixapi.Tag{},
		&ixapi.Relationship{},
		//&ixapi.Item{},
		&ixapi.SearchResult{},
		&ixapi.PanelSearchResultTag{},
	); err != nil {
		return nil, err
	}

	return &IntelXDownloader{
		Term:     	term,
		ZipFile:    outZipFile,
		Threads:    3,
		Limit:      1000,
		apiKey: 	apiKey,
		dbName: 	dbName,
		conn: 		c,
		tempFolder: tempFolder,
		ctx: 		context.Background(),
		mutex:      sync.Mutex{},
		results:    make(chan ixapi.SearchResult),
		status:     &IntelXDownloaderStatus{
			TotalFiles: 0,
			Downloaded: 0,
			TotalBytes: 0,
			StateBytes: 0,
			Spin: "",
			Step: "",
			Running: true,
            IsTerminal: tty.IsTerminal(int(os.Stdin.Fd())),
		},
	}, nil
}

func (dwn *IntelXDownloader) Run() *IntelXDownloaderStatus { 

	defer dwn.Close()
	defer dwn.ClearScreen()

	ascii.HideCursor()
	go func() {
		for dwn.status.Running {
			select {
				case <-dwn.ctx.Done():
					return
				default:
		        	dwn.status.Print()
		        	if dwn.status.IsTerminal {
                        time.Sleep(time.Duration(time.Second / 4))
                    }else{
                        time.Sleep(time.Duration(time.Second * 10))
                    }
		    }
        }
    }()

	r := true
	for r {
		c, err := dwn.SearchNext()
		if err != nil {
			return dwn.status
		}

		if c == 0 || c <= int(float64(dwn.Limit) * 0.95){
			r = false
		}
	}

	if dwn.status.TotalFiles == 0 {
		log.Warn("No result found")
	}else{

		dwn.status.Clear()
		log.Info("Writting Info.csv")
	    dwn.status.Step = "Info.csv"
	    err := dwn.WriteInfoCsv()
	    if err != nil {
	        log.Error("Error writting Info.csv", "err", err)
	        return dwn.status
	    }

	    //Compress   
	    dwn.status.Clear()
	    log.Info("Compressing files")
	    dwn.status.Step = "Compressing"
	    log.Debug("Destination", "zip", dwn.ZipFile)

	    entries, err := os.ReadDir(dwn.tempFolder)
	    if err != nil {
	        log.Error("Error getting file list from temp folder", "err", err)
	        return dwn.status
	    }
	 
	 	archive, err := os.Create(dwn.ZipFile)
	    if err != nil {
	        log.Error("Error creating zip file", "err", err)
	        return dwn.status
	    }
	    defer archive.Close()
	    zipWriter := zip.NewWriter(archive)

	    for _, e := range entries {
	    	log.Debug("Compressing", "file", e.Name())
	        f1, err := os.Open(filepath.Join(dwn.tempFolder, e.Name()))
		    if err != nil {
		        log.Error("Error openning file", "file", e.Name(), "err", err)
		    }else{
			    defer f1.Close()

			    w1, err := zipWriter.Create(e.Name())
			    if err != nil {
			        log.Error("Error creatting file at Zip container", "file", e.Name(), "err", err)
			    }else{
				    if _, err := io.Copy(w1, f1); err != nil {
				        log.Error("Error copping file data to Zip container", "file", e.Name(), "err", err)
				    }
				}
			}

	    }
	    zipWriter.Close()

	    log.Info("Leaks saved", "files", dwn.status.TotalFiles, "zip", dwn.ZipFile)
	}

    tools.RemoveFolder(dwn.tempFolder)
	
	return dwn.status
}

func (dwn *IntelXDownloader) WriteInfoCsv() error {
	file, err := os.OpenFile(filepath.Join(dwn.tempFolder, "Info.csv"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	c, err := database.Connection("sqlite:///"+ dwn.dbName, false, false)
	if err != nil {
		log.Error("Error reconnecting to database", "err", err)
		return err
	}

	rows, err := c.Model(&ixapi.SearchResult{}).Rows()
    defer rows.Close()
    if err != nil {
        return err
    }

	writer := csv.NewWriter(file)
	defer writer.Flush()

	//Write header
	val := reflect.ValueOf(ixapi.CsvItem{})
	numField := val.NumField()

	var fieldNames []string
	for i := 0; i < numField; i++ {
		// skip excluded fields
		if tools.SliceHasStr(csvExludedFields, val.Type().Field(i).Name) {
			continue
		}

		// skip slices
		if val.Field(i).Kind() == reflect.Slice {
			continue // Optionally skip slice fields, or handle them differently
		}

		name := val.Type().Field(i).Name
		switch strings.ToLower(name) {
		case "systemid":
			name = "System ID"
		}

		fieldNames = append(fieldNames, name)
	}

	if err := writer.Write(fieldNames); err != nil {
		return err
	}

	//Write content
	var item ixapi.SearchResult
    for rows.Next() {
        c.ScanRows(rows, &item)

        // get values from the item
		val := reflect.ValueOf(*item.GetCsv())
		numField := val.NumField()

		var values []string
		for i := 0; i < numField; i++ {
			// skip excluded fields
			if tools.SliceHasStr(csvExludedFields, val.Type().Field(i).Name) {
				continue
			}

			// skip slices
			if val.Field(i).Kind() == reflect.Slice {
				continue // Optionally skip slice fields, or handle them differently
			}

			values = append(values, fmt.Sprintf("%v", val.Field(i).Interface()))
		}

		if err := writer.Write(values); err != nil {
			return err
		}
    }

    return nil
	
}

func (dwn *IntelXDownloader) ClearScreen() {
	dwn.status.Clear()
	fmt.Fprintf(os.Stderr, "\n")
	dwn.status.Clear()
	fmt.Fprintf(os.Stderr, "\033[A")
}

func (dwn *IntelXDownloader) DownloadResult(api *ixapi.IntelligenceXAPI, searchID uuid.UUID, Limit int) error {
	logger := log.With("searchID", searchID.String())

	tmpDwn, err := tools.CreateDirFromFilename(dwn.tempFolder, "tmp_zip1_" + searchID.String())
	if err != nil {
        logger.Debug("Error creating temp folder to download zip file", "err", err)
        return err
    }

	fileName := filepath.Join(tmpDwn, tools.SafeFileNameWithRnd(searchID.String()) + ".zip")

	err = api.DownloadZip(dwn.ctx, searchID, Limit, fileName)
	if err != nil {
		logger.Debug("Error downloading data", "err", err)
		return err 
	}

	logger.Debug("Checking downloaded file")
	var mime string
    if mime, err = tools.GetMimeType(fileName); err != nil {
        logger.Debug("Error getting mime type", "err", err)
        return err
    }

    logger.Debug("Mime type", "mime", mime)
    if mime != "application/zip" {
        return errors.New("invalid file type")
    }

    var dst string
    if dst, err = tools.CreateDirFromFilename(dwn.tempFolder, fileName); err != nil {
        logger.Debug("Error creating temp folder to extract zip file", "err", err)
        return err
    }

    if err = tools.Unzip(fileName, dst); err != nil {
        logger.Debug("Error extracting zip file", "temp_folder", dst, "err", err)
        return err
    }
    tools.RemoveFolder(tmpDwn)

    entries, err := os.ReadDir(dst)
    if err != nil {
        return err
    }

    info := filepath.Join(dwn.tempFolder, "Info.csv")
    if tools.FileExists(info) {
        dwn.ParseInfo(info)
    }

    for _, e := range entries {
    	logger.Debug("Checking", "file", e.Name())
        dstFileName := filepath.Join(dwn.tempFolder, e.Name())

        if e.Name() != "Info.csv" {
        	id := strings.Replace(e.Name(), filepath.Ext(e.Name()), "", 1)
        	dwn.conn.Raw("UPDATE intex_result_item SET filename = ?, downloaded = 1 WHERE system_id = ?", e.Name(), id)
        	dwn.status.Downloaded++
        }else{
        	dstFileName = filepath.Join(dwn.tempFolder, "info_orig_" + tools.SafeFileNameWithRnd(searchID.String()) + ".csv")
        }

	    if err = os.Rename(filepath.Join(dst, e.Name()), dstFileName); err != nil {
	        return err
	    }
    }

    tools.RemoveFolder(dst)

	return nil
}

func (dwn *IntelXDownloader) ParseInfo(file_path string) error {
	f, err := os.Open(file_path)
    if err != nil {
        return err
    }
    defer f.Close()

    br := bufio.NewReader(f)
    r, _, err := br.ReadRune()
    if err != nil {
        return err
    }
    if r != '\uFEFF' {
        br.UnreadRune() // Not a BOM -- put the rune back
    }

    csvReader := csv.NewReader(br)
    records, err := csvReader.ReadAll()
    if err != nil {
        return err
    }

    Name := -1
	Date := -1
	Bucket := -1
	Media := -1
	Content := -1
	Type := -1
	Size := -1
	SystemID := -1

	for idx, c := range records[0] {
		switch strings.ToLower(c) {
		    case "name":
		        Name = idx
		    case "date":
		        Date = idx
		    case "bucket":
		        Bucket = idx
		    case "media":
		        Media = idx
		    case "content":
		        Content = idx
		    case "type":
		        Type = idx
		    case "size":
		        Size = idx
		    case "system id":
		        SystemID = idx
		    
	    }
	}

	for idx, rec := range records {
		if idx > 0 {
			s, err := strconv.Atoi(rec[Size])
			if err != nil {
				s = 0
			}

			dt, err := time.Parse("2006-01-02 15:04:05", GetOrDefault(rec, Date, ""))
			if err != nil {
				dt, err = time.Parse(time.RFC3339, GetOrDefault(rec, Date, ""))
				if err != nil {
					dt = time.Now()
				}
			}

			res := &ixapi.SearchResult{
                            MediaH:  	GetOrDefault(rec, Media, ""),
                            TypeH: 		GetOrDefault(rec, Type, ""),
                            BucketH:    GetOrDefault(rec, Bucket, ""),
                        }

            res.SystemID = strings.ToLower(GetOrDefault(rec, SystemID, ""))
            res.Name = GetOrDefault(rec, Name, "")
            res.Bucket = GetOrDefault(rec, Bucket, "")
            res.MediaH = GetOrDefault(rec, Content, "")
            res.Size = int64(s)
            res.Date = dt
            dwn.WriteDb(res, false)
		}
	}

    return nil
}

func GetOrDefault(data []string, index int, def string) string {
	if index == -1 {
		return def
	}

	return strings.Trim(string(data[index]), " \r\n\t")
}

func (dwn *IntelXDownloader) SearchNext() (int, error) {
	var DateFrom, DateTo time.Time
	var inserted int
	var qty int

	DateFrom = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	DateTo = time.Now().UTC()

	wg := sync.WaitGroup{}
	logger := log.With("term", dwn.Term)

	log.Debug("Starting IX Api")
	api := ixapi.IntelligenceXAPI{
		ProxyURL: dwn.ProxyURL,
	}

	api.Init("", dwn.apiKey)

	qty = 0
	response := dwn.conn.Raw("SELECT count(`id`) as qty, min(`date`) as min_date from intex_result_item")
    if response != nil {
    	log.Debug("Response...")
    	
        var mDate time.Time
        var tDate string
        err := response.Row().Scan(&qty, &tDate)
        if err == nil {
        	tDate = tDate[0:10]
        	mDate, err = time.Parse("2006-01-02", tDate)
        	if err == nil {
        		DateTo = mDate.AddDate(0, 0, 1)
        	}
        }else if dwn.status.TotalFiles > 0{
        	log.Debug("Error", "err", err)
        	return 0, err
        }
    }

    log.Info("Quering IntelX Api (" + strconv.Itoa(qty) + " -> " + strconv.Itoa(qty + dwn.Limit) + ")")
    dwn.status.Step = "Searching"

    logger.Debug("Search time", "DateFrom", DateFrom, "DateTo", DateTo)
	searchID, results, selectorInvalid, err := api.SearchWithDates(dwn.ctx, dwn.Term, DateFrom, DateTo, ixapi.SortDateDesc, dwn.Limit, ixapi.DefaultWaitSortTime, ixapi.DefaultTimeoutGetResults)

	if err != nil && selectorInvalid {
		logger.Error("Invalid input selector. Please specify a strong selector")
		log.Warn(textSupportedSelectors)
		return 0, err
	}else if err != nil {
		logger.Error("Error querying results", "err", err)
		return 0, err
	} else if len(results) == 0 && selectorInvalid {
		logger.Error("Invalid input selector. Please specify a strong selector")
		log.Warn(textSupportedSelectors)
		return 0, err
	}

	dwn.status.TotalFiles += len(results)

	if len(results) > 0 {
		logger.Debug("Results", "qty", len(results))

		dwn.results = make(chan ixapi.SearchResult)
	    go func() {
	    	defer close(dwn.results)
			for _, record := range results {
				dwn.results <- record
			}
		}()

		// will spawn Parser.Theads number of "workers" as goroutines
		for w := 0; w < dwn.Threads; w++ {
			wg.Add(1)
		    go func() {
		        defer wg.Done()
		        
		        for dwn.status.Running {
					select {
					case <-dwn.ctx.Done():
						return
					case record, ok := <-dwn.results:
						if !ok || !dwn.status.Running {
							return
						}
						
						logger.Debug("Reg", "did", record.SystemID)

						i, _ := dwn.WriteDb(&record, true)

						if i {
							inserted++
						}else{
							dwn.status.Duplicated++
						}
					}
				}

		    }()
		}

	    wg.Wait()

	    log.Info("Downloading files")
	    dwn.status.Step = "Downloading"

	    downloading := true
	    var dwn_error error
	    wg.Add(1)
		go func() {
	    	defer wg.Done()
			err := dwn.DownloadResult(&api, *searchID, dwn.Limit)
			if err != nil {
				log.Error("Error downloading files", "err", err)
				dwn_error = err
			}
			api.SearchTerminate(context.Background(), *searchID)
			downloading = false
		}()

		wg.Add(1)
		go func() {
	    	defer wg.Done()
			for downloading {
				if api.WriteCounter.Total > 0 {
					dwn.status.StateBytes = int64(api.WriteCounter.Total)
				}
				time.Sleep(time.Duration(time.Second/4))
			}
		}()

	    wg.Wait()

	    if dwn_error != nil {
	    	return 0, dwn_error
	    }

		if api.WriteCounter.Total > 0 {
			dwn.status.StateBytes = 0
			dwn.status.TotalBytes += int64(api.WriteCounter.Total)
		}
	}

    return inserted, nil
}

// Write results to the database
func (dwn *IntelXDownloader) WriteDb(result *ixapi.SearchResult, update bool) (bool, error) {
	dwn.mutex.Lock()
	defer dwn.mutex.Unlock()

	result.Simhash = 0
	//result.Tags = []ixapi.Tag{}
	//result.Relations = []ixapi.Relationship{}

	cnt := -1
	response := dwn.conn.Raw("SELECT count(id) as qty from intex_result_item WHERE system_id = ?", result.SystemID)
    if response != nil {
        _ = response.Row().Scan(&cnt)
    }

    if !update && cnt > 0 {
    	return cnt == 0, nil
    }

    /*
	if _, ok := dwn.conn.Statement.Clauses["ON CONFLICT"]; !ok {
		dwn.conn = dwn.conn.Clauses(clause.OnConflict{UpdateAll: true})
	}*/

	return cnt == 0, dwn.conn.CreateInBatches(result, 50).Error
}

func (dwn *IntelXDownloader) Close() {
	dwn.status.Running = false
}
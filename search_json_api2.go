package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp" // Import regexp package
	"runtime"
	"strings"
	"sync"
	"unicode"
)

// --- MODIFICATION: Regex to find and remove "Leave blank..." lines. ---
// This is compiled once at startup for efficiency.
var leaveBlankRegex = regexp.MustCompile(`(?i)Leave blank\s*\d*\*P[A-Z0-9]+A\d+\*\s*`)

// --- Structs (unchanged) ---
type Match struct {
	FileName string `json:"file_name"`
	Value    string `json:"value"`
}

type FileResult struct {
	DownloadURL string  `json:"download_url"`
	Matches     []Match `json:"matches"`
}

type SearchRequest struct {
	ExamBoard         string `json:"Exam_board"`
	ExamQualifications string `json:"Exam_Qualifications"`
	ExamSubject        string `json:"Exam_Subject"`
	ExamQuery          string `json:"Exam_Query"`
}

// --- Main HTTP handler (unchanged) ---
func main() {
	handler := corsMiddleware(http.HandlerFunc(searchHandler))
	http.Handle("/api/search", handler)

	fmt.Println("Server listening on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Println("Server error:", err)
		os.Exit(1)
	}
}

// --- CORS Middleware (unchanged) ---
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
		if r.Method == "OPTIONS" {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- MODIFIED: searchHandler now searches multiple subject directories ---
func searchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if req.ExamBoard == "" || req.ExamQualifications == "" || req.ExamSubject == "" || req.ExamQuery == "" {
		http.Error(w, "All of Exam_board, Exam_Qualifications, Exam_Subject, and Exam_Query must be provided", http.StatusBadRequest)
		return
	}

	rootDir := "/home/eight/Desktop/QPscan/segregation_8/"

	boardDir, err := findFuzzyDir(rootDir, req.ExamBoard)
	if err != nil {
		http.Error(w, "Exam_board directory not found: "+err.Error(), http.StatusNotFound)
		return
	}

	qualDir, err := findFuzzyDir(boardDir, req.ExamQualifications)
	if err != nil {
		http.Error(w, "Exam_Qualifications directory not found: "+err.Error(), http.StatusNotFound)
		return
	}

	// --- MODIFICATION START: Find all subject directories matching the prefix ---
	subjectDirs, err := findFuzzyDirsByPrefix(qualDir, req.ExamSubject)
	if err != nil {
		http.Error(w, "Error searching for subject directories: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(subjectDirs) == 0 {
		http.Error(w, fmt.Sprintf("No Exam_Subject directories found starting with %q", req.ExamSubject), http.StatusNotFound)
		return
	}

	var jsonFiles []string
	// Walk through each found subject directory to collect all .json files
	for _, subjectDir := range subjectDirs {
		filepath.WalkDir(subjectDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
				jsonFiles = append(jsonFiles, path)
			}
			return nil // Continue walking
		})
	}
	// --- MODIFICATION END ---

	if len(jsonFiles) == 0 {
		http.Error(w, "No .json files found in the matching subject directories", http.StatusNotFound)
		return
	}

	// The rest of the function remains the same, processing the collected jsonFiles
	pattern := buildRelaxedPattern(req.ExamQuery)
	numWorkers := runtime.NumCPU() * 2
	fileCh := make(chan string, len(jsonFiles))
	resCh := make(chan FileResult, len(jsonFiles))
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for file := range fileCh {
			matches, downloadLink, err := processJSONFile(file, pattern)
			if err != nil {
				continue // Or log the error
			}
			if len(matches) > 0 {
				resCh <- FileResult{DownloadURL: downloadLink, Matches: matches}
			}
		}
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker()
	}

	for _, f := range jsonFiles {
		fileCh <- f
	}
	close(fileCh)

	go func() {
		wg.Wait()
		close(resCh)
	}()

	var results []FileResult
	for r := range resCh {
		results = append(results, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// --- processJSONFile (unchanged) ---
func processJSONFile(filename string, re *regexp.Regexp) ([]Match, string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", err
	}
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, "", err
	}

	var downloadLink string
	if rootMap, ok := v.(map[string]interface{}); ok {
		if link, ok := rootMap["__download_link__"].(string); ok {
			downloadLink = link
		} else {
			return nil, "", fmt.Errorf("key '__download_link__' not found or not a string in %s", filename)
		}
	} else {
		return nil, "", fmt.Errorf("root of JSON in %s is not an object", filename)
	}

	baseName := filepath.Base(filename)
	matches := findInValue(v, re, "", baseName)
	return matches, downloadLink, nil
}

// --- findInValue (unchanged) ---
func findInValue(v interface{}, re *regexp.Regexp, path string, fileName string) []Match {
	var out []Match
	switch val := v.(type) {
		case string:
			flat := strings.ReplaceAll(val, "\n", " ")
			if re.MatchString(flat) {
				cleanedValue := leaveBlankRegex.ReplaceAllString(val, "")
				cleanedValue = strings.TrimSpace(cleanedValue)
				if len(cleanedValue) > 0 {
					out = append(out, Match{
						FileName: fileName,
						Value:    cleanedValue,
					})
				}
			}
		case []interface{}:
			for i, e := range val {
				sub := fmt.Sprintf("%s[%d]", path, i)
				out = append(out, findInValue(e, re, sub, fileName)...)
			}
		case map[string]interface{}:
			for k, e := range val {
				if k == "__download_link__" {
					continue
				}
				sub := k
				if path != "" {
					sub = path + "." + k
				}
				out = append(out, findInValue(e, re, sub, fileName)...)
			}
	}
	return out
}

// --- findFuzzyDir (unchanged) ---
func findFuzzyDir(baseDir, tag string) (string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return "", err
	}
	tagNorm := normalizeString(tag)
	for _, entry := range entries {
		if entry.IsDir() {
			dirNorm := normalizeString(entry.Name())
			if strings.Contains(dirNorm, tagNorm) {
				return filepath.Join(baseDir, entry.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("no dir in %q matching tag %q", baseDir, tag)
}

// --- NEW FUNCTION: Finds all directories in a base directory with a given prefix ---
func findFuzzyDirsByPrefix(baseDir, prefix string) ([]string, error) {
	var matchingDirs []string
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}
	prefixNorm := normalizeString(prefix)
	if prefixNorm == "" {
		return matchingDirs, nil // Avoid matching everything if prefix is empty
	}

	for _, entry := range entries {
		if entry.IsDir() {
			dirNorm := normalizeString(entry.Name())
			if strings.HasPrefix(dirNorm, prefixNorm) {
				matchingDirs = append(matchingDirs, filepath.Join(baseDir, entry.Name()))
			}
		}
	}
	return matchingDirs, nil
}

// --- normalizeString (unchanged) ---
func normalizeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// --- buildRelaxedPattern (unchanged) ---
func buildRelaxedPattern(query string) *regexp.Regexp {
	query = strings.TrimSpace(query)
	var sb strings.Builder
	sb.WriteString("(?i)")
	for i, r := range query {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if sb.Len() > 4 {
				sb.WriteString("[\\s\\p{P}]*")
			}
			continue
		}
		sb.WriteString(regexp.QuoteMeta(string(r)))
		if i < len(query)-1 {
			sb.WriteString("[\\s\\p{P}]*")
		}
	}
	pat := sb.String()
	re, err := regexp.Compile(pat)
	if err != nil {
		return regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	}
	return re
}

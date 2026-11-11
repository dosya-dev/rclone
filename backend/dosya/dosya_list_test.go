package dosya

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeListServer serves GET /api/files the way the API does: files paged at
// per_page, every folder repeated on each page
func fakeListServer(t *testing.T, totalFiles int, pages *[]int) rtFunc {
	return func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/files", r.URL.Path)
		q := r.URL.Query()
		assert.Equal(t, "name", q.Get("sort"))
		assert.Equal(t, "asc", q.Get("dir"))
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		require.Positive(t, page)
		require.Positive(t, perPage)
		*pages = append(*pages, page)

		var files []string
		for i := (page - 1) * perPage; i < page*perPage && i < totalFiles; i++ {
			files = append(files, fmt.Sprintf(`{"id":"fil_%d","name":"f%05d.txt","size":1}`, i, i))
		}
		totalPages := max(1, (totalFiles+perPage-1)/perPage)
		body := fmt.Sprintf(`{"ok":true,"files":[%s],"folders":[{"id":"fld_1","name":"sub"}],`+
			`"pagination":{"page":%d,"per_page":%d,"total_files":%d,"total_pages":%d}}`,
			strings.Join(files, ","), page, perPage, totalFiles, totalPages)
		return jsonResp(http.StatusOK, body), nil
	}
}

func TestListFilesAndFoldersFollowsPages(t *testing.T) {
	var pages []int
	f := newFakeFs(fakeListServer(t, 2*listPageSize+7, &pages))

	listing, err := f.listFilesAndFolders(context.Background(), "fld_parent")
	require.NoError(t, err)

	assert.Equal(t, []int{1, 2, 3}, pages)
	require.Len(t, listing.Files, 2*listPageSize+7)
	assert.Equal(t, "f01006.txt", listing.Files[len(listing.Files)-1].Name)
	// folders repeat on every page and must not be duplicated
	assert.Len(t, listing.Folders, 1)
}

func TestListFilesAndFoldersSinglePage(t *testing.T) {
	for _, total := range []int{0, 1, listPageSize} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			var pages []int
			f := newFakeFs(fakeListServer(t, total, &pages))

			listing, err := f.listFilesAndFolders(context.Background(), rootID)
			require.NoError(t, err)

			assert.Equal(t, []int{1}, pages)
			assert.Len(t, listing.Files, total)
		})
	}
}

// shrinkingListServer serves names f00000..f(n-1) where n starts at before
// and drops to after once page 1 has been served `tornPasses` times: the
// deletes land between page reads, which is what tore the listing.
func shrinkingListServer(t *testing.T, before, after, tornPasses int, pages *[]int) rtFunc {
	page1Served := 0
	return func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		*pages = append(*pages, page)
		live := before
		if page == 1 {
			page1Served++
			if page1Served > tornPasses {
				live = after
			}
		} else {
			live = after
		}
		// the deleted rows are the first before-after names, so every later
		// row shifts back by that many
		var files []string
		for i := (page - 1) * perPage; i < page*perPage && i < live; i++ {
			n := i + before - live
			files = append(files, fmt.Sprintf(`{"id":"fil_%d","name":"f%05d.txt","size":1}`, n, n))
		}
		totalPages := max(1, (live+perPage-1)/perPage)
		body := fmt.Sprintf(`{"ok":true,"files":[%s],"folders":[],`+
			`"pagination":{"page":%d,"per_page":%d,"total_files":%d,"total_pages":%d}}`,
			strings.Join(files, ","), page, perPage, live, totalPages)
		return jsonResp(http.StatusOK, body), nil
	}
}

func TestListFilesAndFoldersRereadsATornListing(t *testing.T) {
	var pages []int
	before, after := 2*listPageSize+50, 2*listPageSize-100
	f := newFakeFs(shrinkingListServer(t, before, after, 1, &pages))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)

	// first pass is torn (page 1 at the old total, later pages at the new),
	// the second pass is read whole at the new total
	require.Len(t, listing.Files, after)
	seen := map[string]bool{}
	for _, file := range listing.Files {
		seen[file.Name] = true
	}
	// every live file (the deleted ones are the first before-after names) is
	// listed, and no deleted one is
	for n := before - after; n < before; n++ {
		require.True(t, seen[fmt.Sprintf("f%05d.txt", n)], "live file f%05d.txt missing", n)
	}
	assert.Len(t, seen, after)
}

func TestListFilesAndFoldersGivesUpOnAListingThatNeverSettles(t *testing.T) {
	var pages []int
	f := newFakeFs(shrinkingListServer(t, 2*listPageSize+50, listPageSize+10, 1<<30, &pages))

	_, err := f.listFilesAndFolders(context.Background(), rootID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kept changing")
}

func TestListFilesAndFoldersAcceptsAStableShortCount(t *testing.T) {
	// the server can count a row it then withholds; two agreeing passes win
	var pages []int
	f := newFakeFs(rtFunc(func(r *http.Request) (*http.Response, error) {
		pages = append(pages, 1)
		return jsonResp(http.StatusOK, `{"ok":true,"files":[{"id":"fil_1","name":"a.txt","size":1}],"folders":[],`+
			`"pagination":{"page":1,"per_page":500,"total_files":2,"total_pages":1}}`), nil
	}))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)
	assert.Len(t, listing.Files, 1)
	assert.Len(t, pages, 2)
}

// keysetListServer serves GET /api/files the way a server with keyset paging
// does: an empty cursor asks for the first page, every page answers
// next_cursor (null once the listing is done), and a page starts strictly
// after the cursor's (name, id). The cursor format is the fake's own, which
// is fine because the client must treat it as opaque. afterPage runs once
// each page has been served, so a test can change the directory between
// two page reads.
type keysetFile struct{ id, name string }

func keysetListServer(t *testing.T, files *[]keysetFile, afterPage func(served int)) rtFunc {
	served := 0
	return func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/files", r.URL.Path)
		q := r.URL.Query()
		require.True(t, q.Has("cursor"), "a keyset server is only asked with a cursor parameter")
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		require.Positive(t, perPage)

		live := append([]keysetFile(nil), (*files)...)
		sort.Slice(live, func(i, j int) bool {
			if live[i].name != live[j].name {
				return live[i].name < live[j].name
			}
			return live[i].id < live[j].id
		})
		start := 0
		if c := q.Get("cursor"); c != "" {
			name, id, ok := strings.Cut(c, "|")
			require.True(t, ok)
			start = sort.Search(len(live), func(i int) bool {
				return live[i].name > name || (live[i].name == name && live[i].id > id)
			})
		}
		end := min(start+perPage, len(live))
		var out []string
		for _, file := range live[start:end] {
			out = append(out, fmt.Sprintf(`{"id":%q,"name":%q,"size":1}`, file.id, file.name))
		}
		next := "null"
		if end-start == perPage {
			last := live[end-1]
			b, _ := json.Marshal(last.name + "|" + last.id)
			next = string(b)
		}
		body := fmt.Sprintf(`{"ok":true,"files":[%s],"folders":[{"id":"fld_1","name":"sub"}],`+
			`"pagination":{"page":1,"per_page":%d,"total_files":%d,"total_pages":1,"next_cursor":%s}}`,
			strings.Join(out, ","), perPage, len(live), next)
		served++
		if afterPage != nil {
			afterPage(served)
		}
		return jsonResp(http.StatusOK, body), nil
	}
}

func keysetFiles(n int) []keysetFile {
	files := make([]keysetFile, n)
	for i := range files {
		files[i] = keysetFile{id: fmt.Sprintf("fil_%d", i), name: fmt.Sprintf("f%05d.txt", i)}
	}
	return files
}

func TestListFilesAndFoldersFollowsTheCursor(t *testing.T) {
	files := keysetFiles(2*listPageSize + 7)
	pages := 0
	f := newFakeFs(keysetListServer(t, &files, func(served int) { pages = served }))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)

	assert.Equal(t, 3, pages)
	require.Len(t, listing.Files, 2*listPageSize+7)
	assert.Equal(t, "f01006.txt", listing.Files[len(listing.Files)-1].Name)
	assert.Len(t, listing.Folders, 1)
}

func TestListFilesAndFoldersKeysetSurvivesARenameDuringPaging(t *testing.T) {
	// A file already listed on page 1 is renamed past the end of the
	// directory before page 2 is read. total_files does not move, so the
	// offset re-read cannot see it, and offset paging would start page 2 one
	// row late and silently skip the file that slid into its first slot.
	// The cursor pages after the last name read, so nothing untouched is
	// skipped, and the renamed file is listed once, under its new name.
	n := 2*listPageSize + 7
	files := keysetFiles(n)
	f := newFakeFs(keysetListServer(t, &files, func(served int) {
		if served == 1 {
			files[0].name = "zzz-renamed.txt"
		}
	}))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)

	byID := map[string]string{}
	for _, file := range listing.Files {
		_, dup := byID[file.ID]
		require.False(t, dup, "file %s listed twice", file.ID)
		byID[file.ID] = file.Name
	}
	require.Len(t, byID, n)
	for i := 1; i < n; i++ {
		assert.Equal(t, fmt.Sprintf("f%05d.txt", i), byID[fmt.Sprintf("fil_%d", i)], "untouched file fil_%d", i)
	}
	assert.Equal(t, "zzz-renamed.txt", byID["fil_0"])
}

func TestListFilesAndFoldersKeysetSurvivesADeletePlusUpload(t *testing.T) {
	// One row on page 1 is deleted and a new one uploaded before page 2:
	// the total stays the same, which is the shape offset paging accepted
	// with a live file missing.
	n := 2*listPageSize + 7
	files := keysetFiles(n)
	f := newFakeFs(keysetListServer(t, &files, func(served int) {
		if served == 1 {
			files = append(files[1:], keysetFile{id: "fil_new", name: "f99999.txt"})
		}
	}))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)

	seen := map[string]bool{}
	for _, file := range listing.Files {
		seen[file.ID] = true
	}
	for i := 1; i < n; i++ {
		require.True(t, seen[fmt.Sprintf("fil_%d", i)], "live file fil_%d missing", i)
	}
	assert.True(t, seen["fil_new"])
}

func TestListFilesAndFoldersRereadsWhenTheCursorStopsMidPass(t *testing.T) {
	// An upgrade rolling out mid-listing: page 1 came from a keyset server
	// and page 2 from one that pages by offset. That pass cannot be trusted,
	// so it is read again, and the second pass (all offset) is accepted.
	total := listPageSize + 3
	calls := 0
	f := newFakeFs(rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		var out []string
		for i := (page - 1) * perPage; i < page*perPage && i < total; i++ {
			out = append(out, fmt.Sprintf(`{"id":"fil_%d","name":"f%05d.txt","size":1}`, i, i))
		}
		cursor := ""
		if calls == 1 {
			cursor = fmt.Sprintf(`,"next_cursor":"f%05d.txt|fil_%d"`, perPage-1, perPage-1)
		}
		body := fmt.Sprintf(`{"ok":true,"files":[%s],"folders":[],`+
			`"pagination":{"page":%d,"per_page":%d,"total_files":%d,"total_pages":2%s}}`,
			strings.Join(out, ","), page, perPage, total, cursor)
		return jsonResp(http.StatusOK, body), nil
	}))

	listing, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)
	assert.Len(t, listing.Files, total)
	assert.Equal(t, 4, calls)
}

func TestListFilesAndFoldersAsksForTheCursorOnlyOnTheFirstOffsetPage(t *testing.T) {
	var asked []bool
	f := newFakeFs(rtFunc(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.Query().Has("cursor"))
		return fakeListServer(t, 2*listPageSize+7, new([]int))(r)
	}))

	_, err := f.listFilesAndFolders(context.Background(), rootID)
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false, false}, asked)
}

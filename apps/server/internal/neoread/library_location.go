package neoread

import (
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// CopyLibrary runs after the desktop server exits. The source remains a backup;
// the caller switches its saved location only after this succeeds.
func CopyLibrary(source, destination string) (err error) {
	// Fingerprints use the paths passed to the running Store. Keep their spelling
	// even when symlinks are resolved below for overlap checks and safe copying.
	sourceRoot, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	destinationRoot, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	// OpenStore treats ? as the start of SQLite DSN parameters on restart.
	if strings.Contains(destinationRoot, "?") {
		return fmt.Errorf("书库路径不能包含 ?，请选择其他位置。")
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return err
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	contains := func(root, path string) bool {
		relative, err := filepath.Rel(root, path)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
	}
	if contains(source, destination) || contains(destination, source) {
		return fmt.Errorf("新书库不能与当前书库重叠")
	}
	info, err := os.Lstat(filepath.Join(source, "neoread.sqlite"))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("书库数据库不是普通文件")
	}
	unlock, err := LockLibrary(source)
	if err != nil {
		return err
	}
	defer unlock()
	// Never merge with or overwrite a directory the user already owns.
	if err = os.Mkdir(destination, 0700); err != nil {
		return fmt.Errorf("无法创建新书库，请选择不含 NeoRead Library 的文件夹：%w", err)
	}
	unlockTarget, err := LockLibrary(destination)
	if err != nil {
		_ = os.Remove(destination)
		return err
	}
	defer func() {
		unlockTarget()
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		if relative == "ai-work" {
			return filepath.SkipDir
		}
		if relative == "neoread.lock" || relative == "server.json" {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("书库包含链接或特殊文件，无法安全迁移：%s", relative)
		}
		return copyLibraryFile(path, target)
	})
	if err != nil {
		return err
	}
	// This is an independent library writer. Native Claude files and ownership
	// stay with the source; visible NeoRead history is replayed on first use.
	if err = clearCopiedClaudeSessions(destinationRoot); err != nil {
		return err
	}
	// Validate the copied SQLite database (including any recovered WAL) without
	// running migrations or modifying the source database.
	return checkLibraryDatabase(sourceRoot, destinationRoot)
}

func clearCopiedClaudeSessions(root string) error {
	db, err := sql.Open("sqlite", libraryDatabaseURI(filepath.Join(root, "neoread.sqlite")))
	if err != nil {
		return err
	}
	defer db.Close()
	var exists int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='claude_history_sessions'").Scan(&exists); err != nil || exists == 0 {
		return err
	}
	_, err = db.Exec("DELETE FROM claude_history_sessions; DELETE FROM document_conversations WHERE " + claudeNativeConversationFilter + "; DELETE FROM library_document_conversations WHERE " + claudeNativeConversationFilter)
	return err
}
func copyLibraryFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func libraryDatabaseURI(path string) string {
	sqlitePath := filepath.ToSlash(path)
	// Match Zotero's existing normalization: drive letters are paths, not hosts.
	if !strings.HasPrefix(sqlitePath, "/") {
		sqlitePath = "/" + sqlitePath
	}
	u := url.URL{Scheme: "file", Path: sqlitePath, RawQuery: "mode=rw"}
	return u.String()
}
func checkLibraryDatabase(source, destination string) error {
	db, err := sql.Open("sqlite", libraryDatabaseURI(filepath.Join(destination, "neoread.sqlite")))
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("新书库数据库校验失败：%w", err)
	}
	if result != "ok" {
		return fmt.Errorf("新书库数据库校验失败")
	}
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='documents'").Scan(&tables); err != nil || tables != 1 {
		return fmt.Errorf("新目录不是有效的 NeoRead 书库")
	}
	return relocateCorpusFingerprints(db, source, destination)
}

// Only rebind indexes that matched the source before relocation. Already stale
// indexes remain stale; content revisions, passages and citations never change.
func relocateCorpusFingerprints(db *sql.DB, source, destination string) error {
	return relocateCorpusFingerprintsAt(db, source, destination, destination)
}

func relocateCorpusFingerprintsAt(db *sql.DB, source, physical, destination string) error {
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='document_corpora'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil
	} // Earlier Reader releases did not have corpus indexes.
	rows, err := db.Query(`SELECT c.document_id,d.type,c.fingerprint FROM document_corpora c JOIN documents d ON d.id=c.document_id`)
	if err != nil {
		return err
	}
	type corpusFile struct {
		document    Document
		fingerprint string
	}
	var corpora []corpusFile
	for rows.Next() {
		var c corpusFile
		if err := rows.Scan(&c.document.ID, &c.document.Type, &c.fingerprint); err != nil {
			rows.Close()
			return err
		}
		corpora = append(corpora, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	original := &Server{Store: &Store{Root: source}}
	moved := &Server{Store: &Store{Root: physical}}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range corpora {
		before, err := original.corpusFingerprint(c.document)
		if err != nil || before != c.fingerprint {
			continue
		}
		after, err := moved.corpusFingerprintAt(c.document, destination)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE document_corpora SET fingerprint=? WHERE document_id=?", after, c.document.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ValidateLibrary checks an existing library without creating or migrating it.
func ValidateLibrary(directory string) error {
	database := filepath.Join(directory, "neoread.sqlite")
	info, err := os.Stat(database)
	if os.IsNotExist(err) {
		database = filepath.Join(directory, "reader.sqlite")
		info, err = os.Stat(database)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("书库数据库不是普通文件")
	}
	unlock, err := LockLibrary(directory)
	if err != nil {
		return err
	}
	defer unlock()
	uri := strings.Replace(libraryDatabaseURI(database), "mode=rw", "mode=ro", 1)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("书库数据库校验失败：%w", err)
	}
	if result != "ok" {
		return fmt.Errorf("书库数据库校验失败")
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("书库外键检查失败：%w", err)
	}
	hasInvalidReference := rows.Next()
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return fmt.Errorf("书库外键检查失败：%w", rowErr)
	}
	if hasInvalidReference {
		return fmt.Errorf("书库存在无效的数据关联（外键检查失败），未切换书库")
	}
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='documents'").Scan(&tables); err != nil || tables != 1 {
		return fmt.Errorf("所选目录不是有效的 NeoRead 书库")
	}
	return nil
}

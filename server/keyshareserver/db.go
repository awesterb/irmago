package keyshareserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/privacybydesign/irmago/internal/keysharecore"

	_ "github.com/jackc/pgx/stdlib"
)

var (
	ErrUserAlreadyExists = errors.New("Cannot create user, username already taken")
	ErrUserNotFound      = errors.New("Could not find specified user")
	ErrInvalidData       = errors.New("Invalid user datastructure passed")
	ErrInvalidRecord     = errors.New("Invalid record in database")
)

type LogEntryType string

const (
	PinCheckRefused = "PIN_CHECK_REFUSED"
	PinCheckSucces  = "PIN_CHECK_SUCCES"
	PinCheckFailed  = "PIN_CHECK_FAILED"
	PinCheckBlocked = "PIN_CHECK_BLOCKED"
	IrmaSession     = "IRMA_SESSION"
)

type KeyshareDB interface {
	NewUser(user KeyshareUserData) (KeyshareUser, error)
	User(username string) (KeyshareUser, error)
	UpdateUser(user KeyshareUser) error

	// Reserve returns (allow, tries, wait, error)
	ReservePincheck(user KeyshareUser) (bool, int, int64, error)
	ClearPincheck(user KeyshareUser) error

	SetSeen(user KeyshareUser) error
	AddLog(user KeyshareUser, eventType LogEntryType, param interface{}) error

	AddEmailVerification(user KeyshareUser, emailAddress, token string) error
}

type KeyshareUser interface {
	Data() *KeyshareUserData
}

type KeyshareUserData struct {
	Username string
	Language string
	Coredata keysharecore.EncryptedKeysharePacket
}

type keyshareMemoryDB struct {
	lock  sync.Mutex
	users map[string]keysharecore.EncryptedKeysharePacket
}

type keyshareMemoryUser struct {
	KeyshareUserData
}

func (m *keyshareMemoryUser) Data() *KeyshareUserData {
	return &m.KeyshareUserData
}

func NewMemoryDatabase() KeyshareDB {
	return &keyshareMemoryDB{users: map[string]keysharecore.EncryptedKeysharePacket{}}
}

func (db *keyshareMemoryDB) User(username string) (KeyshareUser, error) {
	// Ensure access to database is single-threaded
	db.lock.Lock()
	defer db.lock.Unlock()

	// Check and fetch user data
	data, ok := db.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return &keyshareMemoryUser{KeyshareUserData{Username: username, Coredata: data}}, nil
}

func (db *keyshareMemoryDB) NewUser(user KeyshareUserData) (KeyshareUser, error) {
	// Ensure access to database is single-threaded
	db.lock.Lock()
	defer db.lock.Unlock()

	// Check and insert user
	_, exists := db.users[user.Username]
	if exists {
		return nil, ErrUserAlreadyExists
	}
	db.users[user.Username] = user.Coredata
	return &keyshareMemoryUser{KeyshareUserData: user}, nil
}

func (db *keyshareMemoryDB) UpdateUser(user KeyshareUser) error {
	userdata, ok := user.(*keyshareMemoryUser)
	if !ok {
		return ErrInvalidData
	}

	// Ensure access to database is single-threaded
	db.lock.Lock()
	defer db.lock.Unlock()

	// Check and update user.
	_, exists := db.users[userdata.Username]
	if !exists {
		return ErrUserNotFound
	}
	db.users[userdata.Username] = userdata.Coredata
	return nil
}

func (db *keyshareMemoryDB) ReservePincheck(user KeyshareUser) (bool, int, int64, error) {
	// Since this is a testing DB, implementing anything more than always allow creates hastle
	return true, 1, 0, nil
}

func (db *keyshareMemoryDB) ClearPincheck(user KeyshareUser) error {
	// Since this is a testing DB, implementing anything more than always allow creates hastle
	return nil
}

func (db *keyshareMemoryDB) SetSeen(user KeyshareUser) error {
	return nil
}

func (db *keyshareMemoryDB) AddLog(user KeyshareUser, eventType LogEntryType, param interface{}) error {
	return nil
}

func (db *keyshareMemoryDB) AddEmailVerification(user KeyshareUser, emailAddress, token string) error {
	return nil
}

type keysharePostgresDatabase struct {
	db *sql.DB
}

type keysharePostgresUser struct {
	KeyshareUserData
	id int64
}

func (m *keysharePostgresUser) Data() *KeyshareUserData {
	return &m.KeyshareUserData
}

const MAX_PIN_TRIES = 3
const BACKOFF_START = 30

func NewPostgresDatabase(connstring string) (KeyshareDB, error) {
	db, err := sql.Open("pgx", connstring)
	if err != nil {
		return nil, err
	}
	return &keysharePostgresDatabase{
		db: db,
	}, nil
}

func (db *keysharePostgresDatabase) NewUser(user KeyshareUserData) (KeyshareUser, error) {
	res, err := db.db.Query("INSERT INTO irma.users (username, language, coredata, lastSeen, pinCounter, pinBlockDate) VALUES ($1, $2, $3, $4, 0, 0) RETURNING id",
		user.Username,
		user.Language,
		user.Coredata[:],
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer res.Close()
	if !res.Next() {
		return nil, ErrUserAlreadyExists
	}
	var id int64
	err = res.Scan(&id)
	if err != nil {
		return nil, err
	}
	return &keysharePostgresUser{KeyshareUserData: user, id: id}, nil
}

func (db *keysharePostgresDatabase) User(username string) (KeyshareUser, error) {
	rows, err := db.db.Query("SELECT id, username, language, coredata FROM irma.users WHERE username = $1", username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrUserNotFound
	}
	var result keysharePostgresUser
	var ep []byte
	err = rows.Scan(&result.id, &result.Username, &result.Language, &ep)
	if err != nil {
		return nil, err
	}
	if len(ep) != len(result.Coredata[:]) {
		return nil, ErrInvalidRecord
	}
	copy(result.Coredata[:], ep)
	return &result, nil
}

func (db *keysharePostgresDatabase) UpdateUser(user KeyshareUser) error {
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return ErrInvalidData
	}
	res, err := db.db.Exec("UPDATE irma.users SET username=$1, language=$2, coredata=$3 WHERE id=$4",
		userdata.Username,
		userdata.Language,
		userdata.Coredata,
		userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) ReservePincheck(user KeyshareUser) (bool, int, int64, error) {
	// Extract data
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return false, 0, 0, ErrInvalidData
	}

	// Check that account is not blocked already, and if not,
	//  update pinCounter and pinBlockDate
	uprows, err := db.db.Query(`
		UPDATE irma.users
		SET pinCounter = pinCounter+1,
			pinBlockDate = $1+$2*2^GREATEST(0, pinCounter-$3)
		WHERE id=$4 AND pinBlockDate<=$5
		RETURNING pinCounter, pinBlockDate`,
		time.Now().Unix()-1-BACKOFF_START, // Grace time of 2 seconds on pinBlockDate set
		BACKOFF_START,
		MAX_PIN_TRIES-2,
		userdata.id,
		time.Now().Unix())
	if err != nil {
		return false, 0, 0, err
	}
	defer uprows.Close()

	// Check whether we have results
	if !uprows.Next() {
		// if no, then account either does not exist (which would be weird here) or is blocked
		// so request wait timeout
		pinrows, err := db.db.Query("SELECT pinBlockDate FROM irma.users WHERE id=$1", userdata.id)
		if err != nil {
			return false, 0, 0, err
		}
		defer pinrows.Close()
		if !pinrows.Next() {
			return false, 0, 0, ErrUserNotFound
		}
		var wait int64
		err = pinrows.Scan(&wait)
		if err != nil {
			return false, 0, 0, err
		}
		return false, 0, wait - time.Now().Unix(), nil
	}

	// Pin check is allowed (implied since there is a result, so pinBlockDate <= now)
	//  calculate tries remaining and wait time
	var tries int
	var wait int64
	err = uprows.Scan(&tries, &wait)
	if err != nil {
		return false, 0, 0, err
	}
	tries = MAX_PIN_TRIES - tries
	if tries < 0 {
		tries = 0
	}
	return true, tries, wait - time.Now().Unix(), nil
}

func (db *keysharePostgresDatabase) ClearPincheck(user KeyshareUser) error {
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return ErrInvalidData
	}
	res, err := db.db.Exec("UPDATE irma.users SET pinCounter=0, pinBlockDate=0 WHERE id=$1", userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) SetSeen(user KeyshareUser) error {
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return ErrInvalidData
	}
	res, err := db.db.Exec("UPDATE irma.users SET lastSeen = $1 WHERE id = $2", time.Now().Unix(), userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) AddLog(user KeyshareUser, eventType LogEntryType, param interface{}) error {
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return ErrInvalidData
	}

	var encodedParamString *string
	if param != nil {
		encodedParam, err := json.Marshal(param)
		if err != nil {
			return err
		}
		encodedParams := string(encodedParam)
		encodedParamString = &encodedParams
	}

	_, err := db.db.Exec("INSERT INTO irma.log_entry_records (time, event, param, user_id) VALUES ($1, $2, $3, $4)",
		time.Now().Unix(),
		eventType,
		encodedParamString,
		userdata.id)
	return err
}

func (db *keysharePostgresDatabase) AddEmailVerification(user KeyshareUser, emailAddress, token string) error {
	userdata, ok := user.(*keysharePostgresUser)
	if !ok {
		return ErrInvalidData
	}

	_, err := db.db.Exec("INSERT INTO irma.email_verification_tokens (token, email, user_id) VALUES ($1, $2, $3)",
		token,
		emailAddress,
		userdata.id)
	return err
}

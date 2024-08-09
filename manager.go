package yubihsm

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indoff/yubihsm-go/commands"
	"github.com/indoff/yubihsm-go/connector"
	"github.com/indoff/yubihsm-go/securechannel"
)

type (
	// SessionManager manages a pool of authenticated secure sessions with a YubiHSM2
	SessionManager struct {
		session   *securechannel.SecureChannel
		lock      sync.Mutex
		connector connector.Connector
		authKeyID uint16
		password  string

		creationWait sync.WaitGroup
		destroyed    bool
		keepAlive    *time.Timer
		swapping     atomic.Bool

		logLevel int // Level of logs to print
	}
)

var (
	echoPayload = []byte("keepalive")
)

const (
	pingInterval = 15 * time.Second

	LogLevel_None  = 0
	LogLevel_Error = 1
	LogLevel_Warn  = 2
	LogLevel_Info  = 3
	LogLevel_Debug = 4
	LogLevel_Trace = 5
)

// NewSessionManager creates a new instance of the SessionManager with poolSize connections.
// Wait on channel Connected with a timeout to wait for active connections to be ready.
func NewSessionManager(connector connector.Connector, authKeyID uint16, password string, logLevel int) (*SessionManager, error) {
	manager := &SessionManager{
		connector: connector,
		authKeyID: authKeyID,
		password:  password,
		destroyed: false,
		logLevel:  logLevel,
	}

	err := manager.swapSession()
	if err != nil {
		return nil, err
	}

	manager.keepAlive = time.NewTimer(pingInterval)
	go manager.pingRoutine()

	return manager, err
}

func (s *SessionManager) pingRoutine() {
	for range s.keepAlive.C {
		s.logTraceMsg(fmt.Sprintf("Keepalive timer tripped for session %d. Sending echo command.\n", s.session.ID))
		command, _ := commands.CreateEchoCommand(echoPayload)

		resp, err := s.SendEncryptedCommand(command)
		if err == nil {
			parsedResp, matched := resp.(*commands.EchoResponse)
			if !matched {
				err = errors.New("invalid response type")
				s.logErrorMsg(fmt.Sprintf("keepalive echo failed: %s", err.Error()))
			}
			if !bytes.Equal(parsedResp.Data, echoPayload) {
				err = errors.New("echoed data is invalid")
				s.logErrorMsg(fmt.Sprintf("keepalive echo failed: %s", err.Error()))
			}
		} else {
			// Session seems to be dead - reconnect and swap
			s.logDebugMsg(fmt.Sprintf("Keepalive: session %d seems to be dead. Attempting to swap...\n", s.session.ID))
			err = s.swapSession()
			if err != nil {
				s.logErrorMsg(fmt.Sprintf("swapping dead session failed; err=%v", err))
			}
		}

		s.keepAlive.Reset(pingInterval)
	}
}

func (s *SessionManager) swapSession() error {
	if s.session != nil {
		s.logDebugMsg(fmt.Sprintf("Swapping session %d...\n", s.session.ID))
	} else {
		s.logDebugMsg("Swapping session: No ID, brand new session.")
	}
	// Lock swapping process
	isAlreadySwapping := s.swapping.CompareAndSwap(false, true)
	if isAlreadySwapping {
		return errors.New("session already swapping")
	}
	defer func() { s.swapping.Store(false) }()
	s.logDebugMsg("Session locked. Now swapping...")

	s.logDebugMsg("Opening new secure channel...")
	newSession, err := securechannel.NewSecureChannel(s.connector, s.authKeyID, s.password)
	if err != nil {
		s.logErrorMsg(fmt.Sprintf("Failed to open new secure channel: %s\n", err.Error()))
		return err
	}
	s.logDebugMsg("Successfully opened new secure channel.")

	s.logDebugMsg("Authenticating new session...")
	err = newSession.Authenticate()
	if err != nil {
		s.logErrorMsg(fmt.Sprintf("Failed to authenticate new session: %s\n", err.Error()))
		return err
	}
	s.logDebugMsg("Successfully authenticated new session.")

	s.lock.Lock()
	defer s.lock.Unlock()
	if s.session != nil {
		s.logDebugMsg(fmt.Sprintf("Swapping session %d: Locked session.\n", s.session.ID))
	} else {
		s.logDebugMsg("Swapping session: Brand new session. Locked session.")
	}

	// Close old session (must be unlocked first)
	if s.session != nil {
		s.logDebugMsg(fmt.Sprintf("Swapping session %d: Closing old session.\n", s.session.ID))
		go func() {
			originalId := s.session.ID
			s.logDebugMsg(fmt.Sprintf("Closing session %d...\n", s.session.ID))
			err := s.session.Close()
			if err != nil {
				s.logErrorMsg(fmt.Sprintf("ERROR failed to close session: %s\n", err.Error()))
			}
			s.logDebugMsg(fmt.Sprintf("Closed session %d\n", originalId))
		}()
	}

	// Replace primary session
	s.session = newSession

	return nil
}

func (s *SessionManager) checkSessionHealth() {
	s.logTraceMsg(fmt.Sprintf("Health check: Session %d: %d / %d messages used.\n", s.session.ID, s.session.Counter, securechannel.MaxMessagesPerSession))
	if s.session.Counter >= securechannel.MaxMessagesPerSession*0.9 && !s.swapping.Load() {
		s.logDebugMsg(fmt.Sprintf("Health check: Session %d: %d / %d messages used. SWAPPING!\n", s.session.ID, s.session.Counter, securechannel.MaxMessagesPerSession))
		go func() {
			err := s.swapSession()
			if err != nil {
				s.logErrorMsg(fmt.Sprintf("failed to swap session: %s\n", err.Error()))
			}
		}()
	}
}

// Logs a message at error level.
//
// msg : The message to log.
func (s *SessionManager) logErrorMsg(msg string) {
	if s.logLevel >= LogLevel_Error {
		log.Print("ERROR " + msg)
	}
}

// Logs a message at debug level.
//
// msg : The message to log.
func (s *SessionManager) logDebugMsg(msg string) {
	if s.logLevel >= LogLevel_Debug {
		log.Print("DEBUG " + msg)
	}
}

// Logs a message at trace level.
//
// msg : The message to log.
func (s *SessionManager) logTraceMsg(msg string) {
	if s.logLevel >= LogLevel_Trace {
		log.Print("TRACE " + msg)
	}
}

// SendEncryptedCommand sends an encrypted & authenticated command to the HSM
// and returns the decrypted and parsed response.
func (s *SessionManager) SendEncryptedCommand(c *commands.CommandMessage) (commands.Response, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	// Check session health after executing the command
	defer s.checkSessionHealth()

	if s.destroyed {
		return nil, errors.New("sessionmanager has already been destroyed")
	}
	if s.session == nil {
		return nil, errors.New("no session available")
	}

	return s.session.SendEncryptedCommand(c)
}

// SendCommand sends an unauthenticated command to the HSM and returns the parsed response
func (s *SessionManager) SendCommand(c *commands.CommandMessage) (commands.Response, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.destroyed {
		return nil, errors.New("sessionmanager has already been destroyed")
	}
	if s.session == nil {
		return nil, errors.New("no session available")
	}

	return s.session.SendCommand(c)
}

// Destroy closes all connections in the pool.
// SessionManager instances can't be reused.
func (s *SessionManager) Destroy() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.keepAlive.Stop()
	s.session.Close()
	s.destroyed = true
}

// Returns the session ID. Will return 0 as the ID if there is an error.
func (s *SessionManager) GetSessionID() (uint8, error) {
	if s.session != nil {
		return s.session.ID, nil
	}
	return 0, errors.New("couldn't retrieve session id: no session is open")
}

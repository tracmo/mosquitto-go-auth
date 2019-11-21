package main

import "C"

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	b64 "encoding/base64"

	"plugin"

	goredis "github.com/go-redis/redis"
	bes "github.com/iegomez/mosquitto-go-auth/backends"
)

type Backend interface {
	GetUser(username, password string) bool
	GetSuperuser(username string) bool
	CheckAcl(username, topic, clientId string, acc int32) bool
	GetName() string
	Halt()
}

type CommonData struct {
	Backends         map[string]Backend
	Plugin           *plugin.Plugin
	PInit            func(map[string]string, log.Level) error
	PGetName         func() string
	PGetUser         func(username, password string) bool
	PGetSuperuser    func(username string) bool
	PCheckAcl        func(username, topic, clientid string, acc int) bool
	PHalt            func()
	Superusers       []string
	AclCacheSeconds  int64
	AuthCacheSeconds int64
	UseCache         bool
	RedisCache       *goredis.Client
	CheckPrefix      bool
	Prefixes         map[string]string
	LogLevel         log.Level
	LogDest          string
	LogFile          string
}

//Cache stores necessary values for Redis cache
type Cache struct {
	Host     string
	Port     string
	Password string
	DB       int32
}

var allowedBackends = map[string]bool{
	"postgres": true,
	"jwt":      true,
	"redis":    true,
	"http":     true,
	"files":    true,
	"mysql":    true,
	"sqlite":   true,
	"mongo":    true,
	"plugin":   true,
	"grpc":     true,
}

var backends []string          //List of selected backends.
var authOpts map[string]string //Options passed by mosquitto.
var cache Cache                //Cache conf.
var commonData CommonData      //General struct with options and conf.
var startupAllGoTime int64     //Tracking the system initialization time so the auth can have the first few minutes in all-go condition

// when Mosquitto starts up, authentication for the first few minutes is in all-go status
// this is to prevent all T4 attempts to get in which causes congestion failure
const AuthAllGoDuration int64 = 60

//export AuthPluginInit
func AuthPluginInit(keys []string, values []string, authOptsNum int) {

	//Initialize Cache with default values
	cache = Cache{
		Host:     "localhost",
		Port:     "6379",
		Password: "",
		DB:       3,
	}

	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	superusers := make([]string, 10, 10)

	cmbackends := make(map[string]Backend)

	//Initialize common struct with default and given values
	commonData = CommonData{
		Superusers:       superusers,
		AclCacheSeconds:  30,
		AuthCacheSeconds: 30,
		CheckPrefix:      false,
		Prefixes:         make(map[string]string),
		LogLevel:         log.InfoLevel,
	}

	//First, get backends
	backendsOk := false
	authOpts = make(map[string]string)
	for i := 0; i < authOptsNum; i++ {
		if keys[i] == "backends" {
			backends = strings.Split(strings.Replace(values[i], " ", "", -1), ",")
			if len(backends) > 0 {
				backendsCheck := true
				for _, backend := range backends {
					if _, ok := allowedBackends[backend]; !ok {
						backendsCheck = false
						log.Errorf("backend not allowed: %s", backend)
					}
				}
				backendsOk = backendsCheck
			}
		} else {
			authOpts[keys[i]] = values[i]
		}
	}

	//Log and end program if backends are wrong
	if !backendsOk {
		log.Fatal("\nbackends error\n")
	}

	//Check if log level is given. Set level if any valid option is given.
	if logLevel, ok := authOpts["log_level"]; ok {

		logLevel = strings.Replace(logLevel, " ", "", -1)

		switch logLevel {
		case "debug":
			commonData.LogLevel = log.DebugLevel
		case "info":
			commonData.LogLevel = log.InfoLevel
		case "warn":
			commonData.LogLevel = log.WarnLevel
		case "error":
			commonData.LogLevel = log.ErrorLevel
		case "fatal":
			commonData.LogLevel = log.FatalLevel
		case "panic":
			commonData.LogLevel = log.PanicLevel
		default:
			log.Info("log_level unkwown, using default info level")
		}

	}

	if logDest, ok := authOpts["log_dest"]; ok {
		switch logDest {
		case "stdout":
			log.SetOutput(os.Stdout)
		case "file":
			if logFile, ok := authOpts["log_file"]; ok {
				file, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err == nil {
					log.SetOutput(file)
				} else {
					log.Errorf("failed to log to file, using default stderr: %s", err)
				}
			}
		default:
			log.Info("log_dest unknown, using default stderr")
		}
	}

	//Initialize backends
	for _, bename := range backends {
		var beIface Backend
		var bErr error

		if bename == "plugin" {
			plug, plErr := plugin.Open(authOpts["plugin_path"])
			if plErr != nil {
				log.Errorf("Could not init custom plugin: %s", plErr)
				commonData.Plugin = nil
			} else {
				commonData.Plugin = plug

				plInit, piErr := commonData.Plugin.Lookup("Init")

				if piErr != nil {
					log.Errorf("Couldn't find func Init in plugin: %s", plErr)
					commonData.Plugin = nil
					continue
				}

				initFunc := plInit.(func(authOpts map[string]string, logLevel log.Level) error)

				ipErr := initFunc(authOpts, commonData.LogLevel)
				if ipErr != nil {
					log.Errorf("Couldn't init plugin: %s", ipErr)
					commonData.Plugin = nil
					continue
				}

				commonData.PInit = initFunc

				plName, gErr := commonData.Plugin.Lookup("GetName")

				if gErr != nil {
					log.Errorf("Couldn't find func GetName in plugin: %s", gErr)
					commonData.Plugin = nil
					continue
				}

				nameFunc := plName.(func() string)
				commonData.PGetName = nameFunc

				plGetUser, pgErr := commonData.Plugin.Lookup("GetUser")

				if pgErr != nil {
					log.Errorf("Couldn't find func GetUser in plugin: %s", pgErr)
					commonData.Plugin = nil
					continue
				}

				getUserFunc := plGetUser.(func(username, password string) bool)
				commonData.PGetUser = getUserFunc

				if pgErr != nil {
					log.Errorf("Couldn't find func GetUser in plugin: %s", pgErr)
					commonData.Plugin = nil
					continue
				}

				plGetSuperuser, psErr := commonData.Plugin.Lookup("GetSuperuser")

				if psErr != nil {
					log.Errorf("Couldn't find func GetSuperuser in plugin: %s", psErr)
					commonData.Plugin = nil
					continue
				}

				getSuperuserFunc := plGetSuperuser.(func(username string) bool)
				commonData.PGetSuperuser = getSuperuserFunc

				plCheckAcl, pcErr := commonData.Plugin.Lookup("CheckAcl")

				if pcErr != nil {
					log.Errorf("Couldn't find func CheckAcl in plugin: %s", pcErr)
					commonData.Plugin = nil
					continue
				}

				checkAclFunc := plCheckAcl.(func(username, topic, clientid string, acc int) bool)
				commonData.PCheckAcl = checkAclFunc

				plHalt, phErr := commonData.Plugin.Lookup("Halt")

				if phErr != nil {
					log.Errorf("Couldn't find func Halt in plugin: %s", phErr)
					commonData.Plugin = nil
					continue
				}

				haltFunc := plHalt.(func())
				commonData.PHalt = haltFunc

				log.Infof("Backend registered: %s", commonData.PGetName())

			}
		} else {
			switch bename {
			case "postgres":
				beIface, bErr = bes.NewPostgres(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["postgres"] = beIface.(bes.Postgres)
				}
			case "jwt":
				beIface, bErr = bes.NewJWT(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["jwt"] = beIface.(bes.JWT)
				}
			case "files":
				beIface, bErr = bes.NewFiles(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["files"] = beIface.(bes.Files)
				}
			case "redis":
				beIface, bErr = bes.NewRedis(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["redis"] = beIface.(bes.Redis)
				}
			case "mysql":
				beIface, bErr = bes.NewMysql(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["mysql"] = beIface.(bes.Mysql)
				}
			case "http":
				beIface, bErr = bes.NewHTTP(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["http"] = beIface.(bes.HTTP)
				}
			case "sqlite":
				beIface, bErr = bes.NewSqlite(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["sqlite"] = beIface.(bes.Sqlite)
				}
			case "mongo":
				beIface, bErr = bes.NewMongo(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["mongo"] = beIface.(bes.Mongo)
				}
			case "grpc":
				beIface, bErr = bes.NewGRPC(authOpts, commonData.LogLevel)
				if bErr != nil {
					log.Fatalf("Backend register error: couldn't initialize %s backend with error %s.", bename, bErr)
				} else {
					log.Infof("Backend registered: %s", beIface.GetName())
					cmbackends["grpc"] = beIface.(bes.GRPC)
				}
			}
		}

	}

	if cache, ok := authOpts["cache"]; ok && strings.Replace(cache, " ", "", -1) == "true" {
		log.Info("Cache activated")
		commonData.UseCache = true
	} else {
		log.Info("No cache set.")
		commonData.UseCache = false
	}

	if commonData.UseCache {
		if cacheHost, ok := authOpts["cache_host"]; ok {
			cache.Host = cacheHost
		}

		if cachePort, ok := authOpts["cache_port"]; ok {
			cache.Port = cachePort
		}

		if cachePassword, ok := authOpts["cache_password"]; ok {
			cache.Password = cachePassword
		}

		if cacheDB, ok := authOpts["cache_db"]; ok {
			db, err := strconv.ParseInt(cacheDB, 10, 32)
			if err == nil {
				cache.DB = int32(db)
			} else {
				log.Warningf("couldn't parse cache db (err: %s), defaulting to %d", err, cache.DB)
			}
		}

		if authCacheSec, ok := authOpts["auth_cache_seconds"]; ok {
			authSec, err := strconv.ParseInt(authCacheSec, 10, 64)
			if err == nil {
				commonData.AuthCacheSeconds = authSec
			} else {
				log.Warningf("couldn't parse AuthCacheSeconds (err: %s), defaulting to %d", err, commonData.AuthCacheSeconds)
			}

		}

		if aclCacheSec, ok := authOpts["acl_cache_seconds"]; ok {
			aclSec, err := strconv.ParseInt(aclCacheSec, 10, 64)
			if err == nil {
				commonData.AclCacheSeconds = aclSec
			} else {
				log.Warningf("couldn't parse AclCacheSeconds (err: %s), defaulting to %d", err, commonData.AclCacheSeconds)
			}

		}

		addr := fmt.Sprintf("%s:%s", cache.Host, cache.Port)

		//If cache is on, try to start redis.
		goredisClient := goredis.NewClient(&goredis.Options{
			Addr:     addr,
			Password: cache.Password, // no password set
			DB:       int(cache.DB),  // use default DB
		})

		_, err := goredisClient.Ping().Result()
		if err != nil {
			log.Errorf("couldn't start Redis, defaulting to no cache. error: %s", err)
			commonData.UseCache = false
		} else {
			commonData.RedisCache = goredisClient
			log.Infof("started cache redis client on DB %d", cache.DB)
			//Check if cache must be reset
			if cacheReset, ok := authOpts["cache_reset"]; ok && cacheReset == "true" {
				commonData.RedisCache.FlushDB()
				log.Infof("flushed cache")
			}
		}

	}

	if checkPrefix, ok := authOpts["check_prefix"]; ok && strings.Replace(checkPrefix, " ", "", -1) == "true" {
		//Check that backends match prefixes.
		if prefixesStr, ok := authOpts["prefixes"]; ok {
			prefixes := strings.Split(strings.Replace(prefixesStr, " ", "", -1), ",")
			if len(prefixes) == len(backends) {
				//Set prefixes
				for i, backend := range backends {
					commonData.Prefixes[prefixes[i]] = backend
				}
				log.Infof("Prefixes enabled for backends %s with prefixes %s.", authOpts["backends"], authOpts["prefixes"])
				commonData.CheckPrefix = true
			} else {
				log.Errorf("Error: got %d backends and %d prefixes, defaulting to prefixes disabled.", len(backends), len(prefixes))
				commonData.CheckPrefix = false
			}

		} else {
			log.Warn("Error: prefixes enabled but no options given, defaulting to prefixes disabled.")
			commonData.CheckPrefix = false
		}
	} else {
		commonData.CheckPrefix = false
	}

	commonData.Backends = cmbackends

}

//export AuthUnpwdCheck
func AuthUnpwdCheck(username, password string) bool {

	// check whether this Mosquitto session just started up
	now := time.Now()
	if startupAllGoTime == 0 {
		startupAllGoTime = now.Unix() + AuthAllGoDuration
		log.Warningf("init the all-go timer to %d", startupAllGoTime)
	}

	// check whether it is all-go time now
	if now.Unix() < startupAllGoTime {
		log.Debugf("it is pwd all-go time for %s", username)
		return true
	}

	// ---------------------------------------------------

	authenticated := false
	var cached = false
	var granted = false
	if commonData.UseCache {
		log.Debugf("checking auth cache for %s", username)
		cached, granted = CheckAuthCache(username, password)
		if cached {
			log.Debugf("found in cache: %s", username)
			return granted
		}
	}

	//If prefixes are enabled, checkt if username has a valid prefix and use the correct backend if so.
	if commonData.CheckPrefix {
		validPrefix, bename := CheckPrefix(username)
		if validPrefix {

			if bename == "plugin" {
				authenticated = CheckPluginAuth(username, password)
			} else {

				var backend = commonData.Backends[bename]

				if backend.GetUser(username, password) {
					authenticated = true
					log.Debugf("user %s authenticated with backend %s", username, backend.GetName())
				}

			}

		} else {
			//If there's no valid prefix, check all backends.
			authenticated = CheckBackendsAuth(username, password)
			//If not authenticated, check for a present plugin
			if !authenticated {
				authenticated = CheckPluginAuth(username, password)
			}
		}
	} else {
		authenticated = CheckBackendsAuth(username, password)
		//If not authenticated, check for a present plugin
		if !authenticated {
			authenticated = CheckPluginAuth(username, password)
		}
	}

	if commonData.UseCache {
		authGranted := "false"
		if authenticated {
			authGranted = "true"
		}
		log.Debugf("setting auth cache for %s", username)
		SetAuthCache(username, password, authGranted)
	}

	return authenticated
}

//export AuthAclCheck
func AuthAclCheck(clientid, username, topic string, acc int) bool {

	// check whether this Mosquitto session just started up
	now := time.Now()
	if startupAllGoTime == 0 {
		startupAllGoTime = now.Unix() + AuthAllGoDuration
		log.Warningf("init the all-go timer to %d", startupAllGoTime)
	}

	// check whether it is all-go time now
	if now.Unix() < startupAllGoTime {
		log.Debugf("it is acl all-go time for %s", username)
		return true
	}

	// ---------------------------------------------------

	aclCheck := false
	var cached = false
	var granted = false
	if commonData.UseCache {
		log.Debugf("checking acl cache for %s", username)
		cached, granted = CheckAclCache(username, topic, clientid, acc)
		if cached {
			log.Debugf("found in cache: %s", username)
			return granted
		}
	}

	//If prefixes are enabled, checkt if username has a valid prefix and use the correct backend if so.
	//Else, check all backends.
	if commonData.CheckPrefix {
		validPrefix, bename := CheckPrefix(username)
		if validPrefix {

			if bename == "plugin" {

				aclCheck = CheckPluginAcl(username, topic, clientid, acc)

			} else {

				var backend = commonData.Backends[bename]

				/*
					// TRACMO: Superuser check is always a false
					log.Debugf("Superuser check with backend %s", backend.GetName())
					if backend.GetSuperuser(username) {
						log.Debugf("superuser %s acl authenticated with backend %s", username, backend.GetName())
						aclCheck = true
					}
				*/

				//If not superuser, check acl.
				if !aclCheck {
					log.Debugf("Acl check with backend %s", backend.GetName())
					if backend.CheckAcl(username, topic, clientid, int32(acc)) {
						log.Debugf("user %s acl authenticated with backend %s", username, backend.GetName())
						aclCheck = true
					}
				}
			}

		} else {
			//If there's no valid prefix, check all backends.
			aclCheck = CheckBackendsAcl(username, topic, clientid, acc)
			//If acl hasn't passed, check for plugin.
			if !aclCheck {
				aclCheck = CheckPluginAcl(username, topic, clientid, acc)
			}
		}
	} else {
		aclCheck = CheckBackendsAcl(username, topic, clientid, acc)
		//If acl hasn't passed, check for plugin.
		if !aclCheck {
			aclCheck = CheckPluginAcl(username, topic, clientid, acc)
		}
	}

	if commonData.UseCache {
		authGranted := "false"
		if aclCheck {
			authGranted = "true"
		}
		log.Debugf("setting acl cache (granted = %s) for %s", authGranted, username)
		SetAclCache(username, topic, clientid, acc, authGranted)
	}

	log.Debugf("Acl is %t for user %s", aclCheck, username)

	return aclCheck
}

//export AuthPskKeyGet
func AuthPskKeyGet() bool {
	return true
}

//CheckAuthCache checks if the username/password pair is present in the cache. Return if it's present and, if so, if it was granted privileges.
func CheckAuthCache(username, password string) (bool, bool) {
	pair := b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("auth%s%s", username, password)))
	val, err := commonData.RedisCache.Get(pair).Result()
	if err != nil {
		return false, false
	}
	//refresh expiration
	commonData.RedisCache.Expire(pair, time.Duration(commonData.AuthCacheSeconds)*time.Second)
	if val == "true" {
		return true, true
	}
	return true, false
}

//SetAuthCache sets a pair, granted option and expiration time.
func SetAuthCache(username, password string, granted string) error {
	pair := b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("auth%s%s", username, password)))
	err := commonData.RedisCache.Set(pair, granted, time.Duration(commonData.AuthCacheSeconds)*time.Second).Err()
	if err != nil {
		return err
	}

	return nil
}

//CheckAclCache checks if the username/topic/clientid/acc mix is present in the cache. Return if it's present and, if so, if it was granted privileges.
func CheckAclCache(username, topic, clientid string, acc int) (bool, bool) {
	pair := b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("acl%s%s%s%d", username, topic, clientid, acc)))
	val, err := commonData.RedisCache.Get(pair).Result()
	if err != nil {
		return false, false
	}
	//refresh expiration
	commonData.RedisCache.Expire(pair, time.Duration(commonData.AclCacheSeconds)*time.Second)
	if val == "true" {
		return true, true
	}
	return true, false
}

//SetAclCache sets a mix, granted option and expiration time.
func SetAclCache(username, topic, clientid string, acc int, granted string) error {
	pair := b64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("acl%s%s%s%d", username, topic, clientid, acc)))
	err := commonData.RedisCache.Set(pair, granted, time.Duration(commonData.AclCacheSeconds)*time.Second).Err()
	if err != nil {
		return err
	}

	return nil
}

//CheckPrefix checks if a username contains a valid prefix. If so, returns ok and the suitable backend name; else, !ok and empty string.
func CheckPrefix(username string) (bool, string) {
	if strings.Index(username, "_") > 0 {
		userPrefix := username[0:strings.Index(username, "_")]
		if prefix, ok := commonData.Prefixes[userPrefix]; ok {
			log.Debugf("Found prefix for user %s, using backend %s.", username, prefix)
			return true, prefix
		}
	}
	return false, ""
}

//CheckBackendsAuth checks for all backends if a username is authenticated and sets the authenticated param.
func CheckBackendsAuth(username, password string) bool {

	authenticated := false

	for _, bename := range backends {

		if bename == "plugin" {
			continue
		}

		var backend = commonData.Backends[bename]

		log.Debugf("checking user %s with backend %s", username, backend.GetName())

		if backend.GetUser(username, password) {
			authenticated = true
			log.Debugf("user %s authenticated with backend %s", username, backend.GetName())
			break
		}
	}

	return authenticated

}

//CheckBackendsAcl  checks for all backends if a username is superuser or has acl rights and sets the aclCheck param.
func CheckBackendsAcl(username, topic, clientid string, acc int) bool {
	//Check superusers first

	aclCheck := false

	/*
		// TRACMO: Superuser check is always a false
		for _, bename := range backends {

			if bename == "plugin" {
				continue
			}

			var backend = commonData.Backends[bename]

			log.Debugf("Superuser check with backend %s", backend.GetName())
			if backend.GetSuperuser(username) {
				log.Debugf("superuser %s acl authenticated with backend %s", username, backend.GetName())
				aclCheck = true
				break
			}
		}
	*/

	if !aclCheck {
		for _, bename := range backends {

			if bename == "plugin" {
				continue
			}

			var backend = commonData.Backends[bename]

			log.Debugf("Acl check with backend %s", backend.GetName())
			if backend.CheckAcl(username, topic, clientid, int32(acc)) {
				log.Debugf("user %s acl authenticated with backend %s", username, backend.GetName())
				aclCheck = true
				break
			}
		}
	}

	return aclCheck

}

//CheckPluginAuth checks that the plugin is not nil and returns the plugins auth response.
func CheckPluginAuth(username, password string) bool {
	if commonData.Plugin != nil {
		return commonData.PGetUser(username, password)
	}
	return false
}

//CheckPluginAcl checks that the plugin is not nil and returns the superuser/acl response.
func CheckPluginAcl(username, topic, clientid string, acc int) bool {
	if commonData.Plugin != nil {
		aclCheck := commonData.PGetSuperuser(username)
		if !aclCheck {
			aclCheck = commonData.PCheckAcl(username, topic, clientid, acc)
		}
	}
	return false
}

//export AuthPluginCleanup
func AuthPluginCleanup() {
	log.Info("Cleaning up plugin")
	//If cache is set, close cache connection.
	if commonData.RedisCache != nil {
		commonData.RedisCache.Close()
	}

	//Halt every registered backend.

	for _, v := range commonData.Backends {
		v.Halt()
	}

	if commonData.Plugin != nil {
		commonData.PHalt()
	}
}

func main() {}

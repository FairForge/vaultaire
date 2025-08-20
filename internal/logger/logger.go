package logger

import "log"

// Simple logger for now
func Info(msg string, args ...interface{}) {
    log.Printf("INFO: "+msg, args...)
}

func Error(msg string, args ...interface{}) {
    log.Printf("ERROR: "+msg, args...)
}

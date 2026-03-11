cwproxy now has new mode called "inspector mode"

inspector mode can be turn on by providing target application's binary like:

./cwproxy {cmd} {...args}
./cwproxy webapp --port 8080

in inspector mode:

1. run provided target application with environment variables and arguments
2. find target application's LISTEN port automatically
3. start proxying application's LISTEN port as normal mode

Q1. how the listen port is detected
A1. i was thinking about we can detect LISTEN port like "ss" command. i don't know how it works but can you implement it in both linux, macos and windows? also it should consider some application not start listening in seconds at start, check LISTEN port multiple times in high frequency like 100ms until there is one.

Q2. what to do if the app opens multiple ports
A2. use most lower number port

Q3. whether explicit APP_PORT should override inspector detection
A3. yes

Q4. child process lifecycle and shutdown behavior details
A4. if child process dead, you should send remaining all metrics and/or logs then exit as child's exit code.

Q5. signal handling between cwproxy and target application
A5. cwproxy should forward all received termination or interrupt signals to target application too, like SIGTERM.

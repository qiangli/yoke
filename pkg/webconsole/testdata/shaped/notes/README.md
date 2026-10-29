# notes — shaped-app fixture

A minimal app of type `shaped`, used by the webconsole tests and by test-host
smoke runs. All data is synthetic.

- `app/` is the app's source tree (tests commit it into a fresh git checkout):
  `public/` is served as static assets, `api/<name>.md` is one Bash# unit per
  endpoint. A unit reads its private data from `$BASHY_APP_DATA` and the
  request from `REQUEST_METHOD`, `QUERY_STRING` and `BASHY_REQUEST_BODY`; its
  stdout is the response.
- `data/<user>/` seeds each user's private data directory.

The units stay within POSIX sh so they also run under a plain `sh` runner.

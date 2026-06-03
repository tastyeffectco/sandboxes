# Phase 2 — minimal shell defaults. Users are expected to edit this.
[ -f /etc/bashrc ] && . /etc/bashrc
[ -f /etc/profile.d/sandbox-env.sh ] && . /etc/profile.d/sandbox-env.sh
export PS1='\u@\h:\w\$ '
alias ll='ls -la'

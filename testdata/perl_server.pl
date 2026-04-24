#!/usr/bin/env perl
use strict;
use warnings;
use Data::ReqRep::Shared;

my ($path, $req_cap, $resp_slots, $resp_data_max, $arena) = @ARGV;
die "usage: perl_server.pl <path> [req_cap [resp_slots [resp_data_max [arena]]]]\n"
    unless defined $path;
$req_cap       //= 256;
$resp_slots    //= 64;
$resp_data_max //= 4096;

my $srv = defined $arena
    ? Data::ReqRep::Shared->new($path, $req_cap, $resp_slots, $resp_data_max, $arena)
    : Data::ReqRep::Shared->new($path, $req_cap, $resp_slots, $resp_data_max);

$| = 1;
print "ready\n";

while (my ($req, $id) = $srv->recv_wait(1.0)) {
    $srv->reply($id, $req);
}

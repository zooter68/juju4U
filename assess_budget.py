#!/usr/bin/env python
"""This will test the budget commands utilized for commercial charm billing.
These commands are linked to a ubuntu one account, and as such, require the
user account to be setup before test execution (including authentication)."""

from __future__ import print_function

import argparse
import logging
import sys
import subprocess
import json
from random import randint

from deploy_stack import (
    BootstrapManager,
    )
from utility import (
    add_basic_testing_arguments,
    JujuAssertionError,
    configure_logging,
    )
from jujupy import (
    client_from_config,
    )


__metaclass__ = type


log = logging.getLogger("assess_budget")

def assert_equal(found, expected):
    found = sorted(found)
    expected = sorted(expected)
    if found != expected:
        raise JujuAssertionError(
            'Found: {}\nExpected: {}'.format(found, expected))

def _get_new_budget_limit(client):
    """Return availible limit for new budget"""
    budgets = json.loads(list_budgets(client))
    log.debug('Found credit limit {}, currently used {}'.format(
        budgets['credit'], budgets['total']['limit']))
    return int(budgets['credit']) - int(budgets['total']['limit'])

def _get_budgets(client):
    budgets = []
    for budget in json.loads(list_budgets(client))['budgets']:
        budgets.append(budget)
    return budgets

def list_budgets(client):
    """Return defined budgets as json."""
    return client.get_juju_output('list-budgets', '--format', 'json', include_e=False)

def show_budget(client, name):
    """Return specified budget as json."""
    return client.get_juju_output('show-budget', name, '--format', 'json', include_e=False)

def create_budget(client, name, value):
    """Create a budget."""
    return client.get_juju_output('create-budget', name, value, include_e=False)

def set_budget(client, name, value):
    """Change an existing budgets allocation."""
    return client.get_juju_output('set-budget', name, value, include_e=False)

def _try_setting_budget(client, name, value):
    try:
        output = set_budget(client, name, value)
    except subprocess.CalledProcessError as e:
        output = [e.output, e.stderr]
        raise JujuAssertionError('Could not set budget {}'.format(output))
    else:
        if 'budget limit updated' in output:
            log.info('Set budget "{}" to value {}'.format(name, value))
            pass
        else:
            raise JujuAssertionError('Error testing set-budget {}'.format(
                output))

def _try_creating_budget(client, name, value):
    try:
        create_budget(client, name, value)
        log.info('Created new budget "{}" with value {}'.format(name,
                                                             value))
    except subprocess.CalledProcessError as e:
        output = [e.output, e.stderr]
        if any('already exists' in message for message in output):
            pass
            log.info('Reusing budget "{}" with value {}'.format(name,value))
            # this should be a failure once lp:1663258 is fixed
            # for initial budget creation
        else:
            raise JujuAssertionError(
                'Error testing create-budget {}'.format(output))
    else:
        raise JujuAssertionError('Added duplicate budget')

def assess_create_budget(client, budget_name, budget_value, budget_limit):
    """Test create-budget command"""
    log.info('create-budget "{}" with value {}, limit {}'.format(budget_name,
                                                                 budget_value,
                                                                 budget_limit))
    
    # Do this twice, to ensure budget exists and we can check for
    # duplicate message. Ideally, once lp:1663258 is fixed, we will
    # assert on initial budget creation as well.
    _try_creating_budget(client, budget_name, budget_value)
    
    # Assert on ERROR failed to create the budget: budget "*" already exists
    _try_creating_budget(client, budget_name, budget_value)

    # check bounds

def assess_set_budget(client, budget_name, budget_value, budget_limit):
    """Test set-budget command"""
    log.info('set-budget "{}" with value {}, limit {}'.format(budget_name,
                                                              budget_value,
                                                              budget_limit))

    # Check some bounds
    # failed to set the budget: budget limits cannot exceed the credit limit
    try:
        _try_setting_budget(client, budget_name, str(budget_limit+1))
    except JujuAssertionError as e:
        if 'exceed the credit limit' in e.message:
            pass
        else:
            raise JujuAssertionError('Error testing credit limit {}'.format(e))
    else:
        raise JujuAssertionError('Credit limit exceeded {}'.format(e))

    _try_setting_budget(client, budget_name, budget_value)

def assess_budget_limit(client, budget_limit):
    log.info('Budget limit found {}'.format(budget_limit))

    if budget_limit < 0:
        raise JujuAssertionError('Negative Budget Limit found')

def assess_show_budget(client, budget_name, budget_value):
    log.info('show-budget "{}" with value {}'.format(budget_name,
                                                     budget_value))
    
    budget = json.loads(show_budget(client, budget_name))
    # assert budget value
    if budget['limit'] != budget_value:
        raise JujuAssertionError('show-budget found {}, expected {}'.format(
            budget['limit'], budget_value))

    # assert on usage (0% until we use it)
    if budget['total']['usage'] != '0%':
        raise JujuAssertionError('show-budget found {}, expected {}'.format(
            budget['total']['usage'], '0%'))


def assess_list_budgets(client, expected_budgets):
    # Since we can't remove budgets until lp:1663258
    # is fixed, we don't modify the list contents or count
    # Nonetheless, we assert on it for future use
    budgets = _get_budgets(client)
    assert_equal(budgets, expected_budgets)

def _set_budget_value_expectations(expected_budgets, name, value):
    # Update our expectations accordingly
    for budget in expected_budgets:
        if budget['budget'] == name:
            # For now, we assume we aren't spending down the budget
            budget['limit'] = unicode(value)
            budget['unallocated'] = unicode(value)
            # .00 is appended to availible for some reason
            budget['available'] = unicode(value+'.00')
            log.info('Expected budget updated: "{}" to value {}'.format(name,
                                                                        value))

def assess_budget(client):
    # Since we can't remove budgets until lp:1663258
    # is fixed, we avoid creating new random budgets and hardcode.
    # We also, zero out the previous budget
    budget_name = 'test'
    _try_setting_budget(client, budget_name, '0')

    budget_limit = _get_new_budget_limit(client)
    expected_budgets = _get_budgets(client)
    
    assess_budget_limit(client, budget_limit)
    budget_value = str(randint(1,budget_limit/2))
    assess_create_budget(client, budget_name, budget_value, budget_limit)

    budget_value = str(randint(budget_limit/2+1,budget_limit))
    _set_budget_value_expectations(expected_budgets, budget_name, budget_value)
    assess_set_budget(client, budget_name, budget_value, budget_limit)
    
    assess_show_budget(client, budget_name, budget_value)
    assess_list_budgets(client, expected_budgets)

def parse_args(argv):
    """Parse all arguments."""
    parser = argparse.ArgumentParser(description="Test budget commands")
    add_basic_testing_arguments(parser)
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    configure_logging(args.verbose)                                                     
    client = client_from_config(args.env, args.juju_bin, False)
    assess_budget(client)
    return 0


if __name__ == '__main__':
    sys.exit(main())
